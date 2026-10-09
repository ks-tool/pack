// Package sdk is what an author of a component writes against: a struct
// whose VALUE is the defaults document, a handful of embeddable pieces every
// component has (the platform half, the scale, claims, secrets), and the
// functions that turn the struct into the artifact — schema.json, pack.yaml,
// templates/ — and check it. The author describes fields; the tool derives
// the rest.
//
//	type PgInit struct {
//	    sdk.Workload                    // scale (single | replicas) and the pack's half of platform (resources, env, metadata)
//	    DB   DB   `json:"db"`
//	    Init Init `json:"init" pack:"file=/opt/pack/pg-init.sh,format=template,template=pg-init.sh.tmpl,parseAs=text,mode=0500"`
//	}
//
//	func (PgInit) Meta() sdk.Meta { return sdk.Meta{Name: "pg-init", Version: "4.3.0", Image: "oci://…/aio:4.3.0", Templates: templates} }
//	func (PgInit) PackRoot() packschema.Root { return packschema.Root{RestartPolicy: "Never", Peers: …} }
//
//	func Defaults() PgInit { return PgInit{Workload: sdk.Single("100m", "128Mi"), …} }
//
// Only Meta is required. PackRoot and PackCompute are optional — see
// packschema. Generate writes the artifact's directory; Verify runs the
// author checks in a test; Run is a ready generator CLI.
package sdk

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	packer "github.com/arenadata/oci-packer/types"
	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/sdk/check"
	"github.com/ks-tool/pack/sdk/packschema"
	"github.com/ks-tool/pack/sdk/schema"
)

// Meta identifies the artifact a component (or a composition) becomes.
type Meta struct {
	Name     string // io.horchestra.name — a DNS label; the key of the artifact among a pack's members
	Version  string // io.horchestra.version
	Image    string // oci://host/repo:tag the component runs; "" for a library or a composition
	Artifact string // oci://host/repo where the artifact is published (no tag): what a pack mounts its members from
	// Templates ships every file of the tree as a template member — the
	// files `format: template` names. Typically `//go:embed templates`.
	Templates fs.FS
	// TemplatesDir is the directory inside Templates holding them ("templates"
	// when the FS is the go:embed of that directory; "" or "." when the FS is
	// the directory itself).
	TemplatesDir string
	// Platform is the image's platform for the pack file (default linux/amd64).
	Platform string
}

// Component is an author's default value: a struct with json and pack tags
// whose value is the defaults document. PackRoot and PackCompute are optional.
type Component interface {
	Meta() Meta
}

// Composition is a pack: a struct whose fields are the keys of the system,
// each `pack_ref:"<name>"`, and the components it mounts.
type Composition interface {
	Component
	Members() []Component
}

// ---- embeddable pieces

// Workload is what every running component has: the scale fact (single or
// replicas — x-pack.scale) and the pack's half of its platform — resources,
// env, metadata. How many replicas is the target's. Embed it; set the
// default with Single or Replicated.
type Workload struct {
	// Scale is lifted into x-pack.scale of the component's root: a fact
	// about the application, not a value anyone writes.
	Scale    string   `json:"-" pack:"scale"`
	Platform Platform `json:"platform" pack:"ref=urn:pack:platform,default"`
}

// Single is a single-instance workload with the given requests and a memory
// limit equal to the request.
func Single(cpu, memory string) Workload {
	return Workload{Scale: "single", Platform: sized(cpu, memory)}
}

// Replicated is a replicated workload — as many replicas as the target
// runs — with the given requests and a memory limit equal to the request.
func Replicated(cpu, memory string) Workload {
	return Workload{Scale: "replicas", Platform: sized(cpu, memory)}
}

func sized(cpu, memory string) Platform {
	return Platform{Resources: Resources{
		Requests: map[string]string{"cpu": cpu, "memory": memory},
		Limits:   map[string]string{"memory": memory},
	}}
}

// Limits sets the memory limit apart from the request.
func (w Workload) Limits(memory string) Workload {
	p := w.Platform
	p.Resources.Limits = map[string]string{"memory": memory}
	w.Platform = p
	return w
}

// Secret is a credential leaf: the operator gives a reference to a Secret,
// never a value.
type Secret = packschema.Secret

// Claim is a persistent volume: the mount path is a fact, size and class settings.
type Claim = packschema.Claim

// ClaimAt is a Claim with a default size.
func ClaimAt(mountPath, size string) Claim { return Claim{MountPath: mountPath, Size: size} }

// ---- the artifact

// Schema is the component's schema.json, stable byte for byte.
func Schema(c Component) ([]byte, error) {
	s, err := packschema.Reflect(c)
	if err != nil {
		return nil, err
	}

	return json.Marshal(s)
}

// Artifact types of a pack's members, as the pack files declare them.
const (
	ArtifactPack            = "application/vnd.horchestra.pack.v1+json"
	ArtifactPackSchema      = "application/vnd.horchestra.pack.schema.v1+json"
	ArtifactComponent       = "application/vnd.horchestra.component.v1+json"
	ArtifactComponentSchema = "application/vnd.horchestra.component.schema.v1+json"
	ArtifactTemplate        = "application/vnd.horchestra.template.v1"

	AnnotationName    = "io.horchestra.name"
	AnnotationVersion = "io.horchestra.version"
)

// PackFile is the artifact's pack file as oci-packer's own type: a
// component's image (a mount), its schema and its templates; a composition's
// schema and the members it mounts. Marshal it with PackFileYAML.
func PackFile(c Component) (*packer.Pack, error) {
	m := c.Meta()
	if len(m.Name) == 0 || len(m.Version) == 0 {
		return nil, fmt.Errorf("%T: Meta needs Name and Version", c)
	}

	p := &packer.Pack{Metadata: packer.Metadata{Annotations: map[string]string{AnnotationName: m.Name, AnnotationVersion: m.Version}}}

	if comp, ok := c.(Composition); ok {
		p.Type = ArtifactPack
		p.Items = append(p.Items, packer.Descriptor{From: "file://schema.json", Type: ArtifactPackSchema})
		for _, member := range comp.Members() {
			mm := member.Meta()
			if len(mm.Artifact) == 0 {
				return nil, fmt.Errorf("%s: member %s has no Artifact (oci://host/repo) to mount it from", m.Name, mm.Name)
			}
			p.Items = append(p.Items, packer.Descriptor{From: mm.Artifact + ":" + mm.Version})
		}
		return p, nil
	}

	p.Type = ArtifactComponent
	if len(m.Image) > 0 {
		platform := m.Platform
		if len(platform) == 0 {
			platform = "linux/amd64"
		}
		p.Items = append(p.Items, packer.Descriptor{From: m.Image, Platform: platform})
	}

	p.Items = append(p.Items, packer.Descriptor{From: "file://schema.json", Type: ArtifactComponentSchema})

	if n, _ := templates(m); len(n) > 0 {
		p.Items = append(p.Items, packer.Descriptor{From: "dir://templates/", Type: ArtifactTemplate})
	}

	return p, nil
}

// PackFileYAML is PackFile as the bytes of pack.yaml.
func PackFileYAML(c Component) ([]byte, error) {
	p, err := PackFile(c)
	if err != nil {
		return nil, err
	}
	return yaml.Marshal(p)
}

// templates lists the files Meta.Templates ships, by their name.
func templates(m Meta) (map[string][]byte, error) {
	out := map[string][]byte{}
	if m.Templates == nil {
		return out, nil
	}

	root := m.TemplatesDir
	if len(root) == 0 {
		root = "."
	}

	err := fs.WalkDir(m.Templates, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(m.Templates, p)
		if err != nil {
			return err
		}
		rel := p
		if root != "." {
			rel = strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		}
		out[rel] = b
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// Generate writes every item's directory under dir: <name>/schema.json,
// <name>/pack.yaml and <name>/templates/*. A composition's members are
// generated too, so one call writes the whole tree oci-packer packs.
func Generate(dir string, items ...Component) error {
	seen := map[string]bool{}
	var all []Component
	var add func(c Component)
	add = func(c Component) {
		name := c.Meta().Name
		if seen[name] {
			return
		}
		seen[name] = true
		all = append(all, c)
		if comp, ok := c.(Composition); ok {
			for _, m := range comp.Members() {
				add(m)
			}
		}
	}

	for _, c := range items {
		add(c)
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Meta().Name < all[j].Meta().Name })
	for _, c := range all {
		m := c.Meta()
		out := filepath.Join(dir, m.Name)
		if err := os.MkdirAll(out, 0o755); err != nil {
			return err
		}

		s, err := Schema(c)
		if err != nil {
			return fmt.Errorf("%s: %w", m.Name, err)
		}

		if err = os.WriteFile(filepath.Join(out, "schema.json"), s, 0o644); err != nil {
			return err
		}

		pf, err := PackFileYAML(c)
		if err != nil {
			return err
		}

		if err = os.WriteFile(filepath.Join(out, "pack.yaml"), pf, 0o644); err != nil {
			return err
		}

		ts, err := templates(m)
		if err != nil {
			return fmt.Errorf("%s: templates: %w", m.Name, err)
		}

		for name, b := range ts {
			p := filepath.Join(out, "templates", name)
			if err = os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err = os.WriteFile(p, b, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// Verify runs the author checks (spec §2.6) on the component as generated:
// the schema, its templates, its libraries. Meant for the author's tests.
func Verify(c Component, libs ...Component) ([]check.Problem, error) {
	raw, err := Schema(c)
	if err != nil {
		return nil, err
	}

	s, err := schema.Parse(raw)
	if err != nil {
		return nil, err
	}

	for _, lib := range libs {
		lraw, err := Schema(lib)
		if err != nil {
			return nil, err
		}
		ls, err := schema.Parse(lraw)
		if err != nil {
			return nil, err
		}
		ls.Foreign = s.Foreign
		s.Attach(packschema.PackScheme+lib.Meta().Name, ls)
	}

	if err = s.AttachBuiltins(); err != nil {
		return nil, err
	}
	_, hasPlatform := s.Root.Properties["platform"]

	ts, err := templates(c.Meta())
	if err != nil {
		return nil, err
	}

	return check.Run(check.Component{Name: c.Meta().Name, Schema: s, Templates: ts, IsLibrary: !hasPlatform})
}

// Run is the generator CLI of an author's main package, over the flag
// package — one FlagSet per subcommand:
//
//	generate [-o dir]   write every artifact's directory (default .)
//	check               run the author checks on every component
//	schema <name>       print one component's schema.json
//
// It returns the error instead of exiting, so main decides:
//
//	func main() {
//	    if err := sdk.Run(postgres.Defaults()); err != nil {
//	        fmt.Fprintln(os.Stderr, err)
//	        os.Exit(1)
//	    }
//	}
func Run(items ...Component) error {
	usage := "usage: generate [-o dir] | check | schema <name>"

	args := os.Args[1:]
	if len(args) == 0 {
		return errors.New(usage)
	}

	byName := map[string]Component{}
	var collect func(c Component)
	collect = func(c Component) {
		byName[c.Meta().Name] = c
		if comp, ok := c.(Composition); ok {
			for _, m := range comp.Members() {
				collect(m)
			}
		}
	}
	for _, c := range items {
		collect(c)
	}

	switch args[0] {
	case "generate":
		flagSet := flag.NewFlagSet("generate", flag.ContinueOnError)
		out := flagSet.String("o", ".", "directory to write the artifacts under")
		if err := flagSet.Parse(args[1:]); err != nil {
			return err
		}
		if err := Generate(*out, items...); err != nil {
			return err
		}
		for _, c := range items {
			fmt.Println("wrote", filepath.Join(*out, c.Meta().Name))
		}
		return nil

	case "check":
		flagSet := flag.NewFlagSet("check", flag.ContinueOnError)
		if err := flagSet.Parse(args[1:]); err != nil {
			return err
		}
		names := make([]string, 0, len(byName))
		for n := range byName {
			names = append(names, n)
		}
		sort.Strings(names)
		problems := 0
		for _, n := range names {
			c := byName[n]
			if _, ok := c.(Composition); ok {
				continue
			}
			var libs []Component
			for _, other := range byName {
				if _, isComp := other.(Composition); !isComp && len(other.Meta().Image) == 0 && other.Meta().Name != n {
					libs = append(libs, other)
				}
			}
			ps, err := Verify(c, libs...)
			if err != nil {
				return fmt.Errorf("%s: %w", n, err)
			}
			for _, p := range ps {
				fmt.Printf("%s: %s\n", n, p)
				problems++
			}
		}
		if problems > 0 {
			return fmt.Errorf("%d problem(s)", problems)
		}
		fmt.Println("ok")
		return nil

	case "schema":
		flagSet := flag.NewFlagSet("schema", flag.ContinueOnError)
		if err := flagSet.Parse(args[1:]); err != nil {
			return err
		}
		if flagSet.NArg() != 1 {
			return errors.New("schema <name>")
		}
		c, ok := byName[flagSet.Arg(0)]
		if !ok {
			return fmt.Errorf("no component %q", flagSet.Arg(0))
		}
		s, err := Schema(c)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(s)
		return err
	}
	return fmt.Errorf("unknown command %q\n%s", args[0], usage)
}
