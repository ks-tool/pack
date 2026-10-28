// Package check is the author's side of the contract: what `pack check` (and
// the loader again, at acceptance) verifies about a component before anything
// is rendered. Every problem is a path and a sentence; the list is complete, not
// first-failure.
package check

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ks-tool/pack/sdk/compute"
	"github.com/ks-tool/pack/sdk/packschema"
	"github.com/ks-tool/pack/sdk/schema"
	"github.com/ks-tool/pack/sdk/semver"
)

type fileAt struct {
	ptr  string
	file *packschema.File
}

type Problem struct {
	Path    string
	Message string
}

func (p Problem) String() string { return p.Path + ": " + p.Message }

// Component is what the checks see of one component.
type Component struct {
	Name      string
	Schema    *schema.Schema
	Templates map[string][]byte
	Image     *ocispec.Image // nil for a library, and for a check run on a directory
	IsLibrary bool
}

// nodeOwned are the directories a node owns; a config there would silently lose.
var nodeOwned = []string{"/etc/resolv.conf", "/etc/passwd", "/etc/group", "/etc/hosts", "/etc/ssl", "/etc/pki", "/proc", "/sys", "/dev", "/run/secrets"}

// Run performs the author checks of spec §2.6 on one component.
func Run(c Component) ([]Problem, error) {
	var ps []Problem
	add := func(ptr, format string, args ...any) { ps = append(ps, Problem{ptr, fmt.Sprintf(format, args...)}) }

	root, err := c.Schema.Root.RootAnnotation()
	if err != nil {
		return nil, err
	}

	// 1. defaults pass the schema; const and default never share a node
	defaults, _, err := c.Schema.Defaults()
	if err != nil {
		add("", "defaults: %v", err)
	} else {
		compiled, err := c.Schema.Compile("pack://" + c.Name)
		if err != nil {
			return nil, fmt.Errorf("schema does not compile: %w", err)
		}
		if defaults == nil {
			defaults = map[string]any{}
		}
		for _, v := range compiled.Validate(defaults) {
			// required leaves without a default are the operator's to name — not an error here
			if strings.Contains(v.Message, "missing propert") || strings.Contains(v.Message, "required") {
				continue
			}
			add(v.Path, "defaults do not pass the schema: %s", v.Message)
		}
	}

	env, err := compute.Env()
	if err != nil {
		return nil, err
	}

	var files []fileAt
	var fileStack []fileAt
	var argLeaves, envLeaves, passwordLeaves []string
	scale := root.Scale
	ports := map[string]bool{}
	insideFile := map[string]string{} // instance ptr → the file subtree it is under
	computes := map[string]string{}
	computeTypes := map[string][]string{} // instance ptr → the node's JSON types

	err = c.Schema.Visit(func(ptr string, s *schema.Schema, n *schema.Node) error {
		// track "is this node under an x-pack-file subtree"
		for len(fileStack) > 0 && !strings.HasPrefix(ptr, fileStack[len(fileStack)-1].ptr+"/") && ptr != fileStack[len(fileStack)-1].ptr {
			fileStack = fileStack[:len(fileStack)-1]
		}
		if len(fileStack) > 0 {
			insideFile[ptr] = fileStack[len(fileStack)-1].ptr
		}
		if n.HasConst && n.HasDefault {
			add(ptr, "const and default on one node — a fact or a setting, not both")
		}
		if n.File != nil {
			if len(fileStack) > 0 && fileStack[len(fileStack)-1].ptr != ptr {
				add(ptr, "x-pack-file inside the file %s", fileStack[len(fileStack)-1].file.Path)
			}
			files = append(files, fileAt{ptr, n.File})
			fileStack = append(fileStack, fileAt{ptr, n.File})
			insideFile[ptr] = ptr
			if n.File.Format == packschema.FormatSecret {
				if !n.IsPassword() {
					add(ptr, "x-pack-file format secret mounts a secret's key as the file: the node must be format: password")
				}
				if len(n.File.Template) > 0 || len(n.File.ParseAs) > 0 {
					add(ptr, "format secret has no template or parseAs: the bytes are the secret's")
				}
			}
		}
		if n.Arg != nil {
			argLeaves = append(argLeaves, ptr)
			if n.IsPassword() {
				add(ptr, "x-pack-arg on a password: /proc/<pid>/cmdline is world-readable — use a flagfile or x-pack-env")
			}
		}
		if len(n.Env) > 0 {
			envLeaves = append(envLeaves, ptr)
		}
		if n.IsPassword() {
			passwordLeaves = append(passwordLeaves, ptr)
		}
		if len(n.Compute) > 0 {
			computes[ptr] = n.Compute
			computeTypes[ptr] = n.Types
			if n.HasDefault {
				// allowed: default is the constant, compute the formula — spec: "оба на одном узле — отказ" refers to const+default; compute+default is the documented pair
			}
		}
		if strings.HasPrefix(ptr, "/ports/") && strings.Count(ptr, "/") == 2 {
			ports[strings.TrimPrefix(ptr, "/ports/")] = true
			if !n.HasConst {
				add(ptr, "a port is a fact: declare it with const")
			}
		}
		if n.IsForeign() && strings.HasPrefix(n.Ref, packschema.PackScheme) {
			lib := strings.TrimSuffix(strings.TrimPrefix(n.Ref, packschema.PackScheme), "#")
			if _, ok := root.Requires[lib]; !ok {
				add(ptr, "library %q is referenced but not declared in x-pack.requires", lib)
			}
			if len(n.Mount) == 0 {
				add(ptr, "library key without x-pack-mount: where do its layers go?")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 2. formulas compile; reads are known; no cycles among them; a library reads no peers
	reads := map[string][]string{}
	for ptr, expr := range computes {
		a, err := compute.Compile(env, expr)
		if err != nil {
			add(ptr, "x-pack-compute does not compile: %v", err)
			continue
		}
		reads[ptr] = compute.Reads(a)
		// the formula's static type against the node's, where the formula fixes one
		if rt, err := compute.ResultType(env, expr); err != nil {
			add(ptr, "x-pack-compute does not type-check: %v", err)
		} else if len(rt) > 0 && len(computeTypes[ptr]) > 0 && !typeAccepts(computeTypes[ptr], rt) {
			add(ptr, "x-pack-compute yields %s, the node is %s", rt, strings.Join(computeTypes[ptr], "|"))
		}
		for _, r := range reads[ptr] {
			if r == "pack.instance" || r == "pack.index" || r == "pack.ordinal" {
				if owner, ok := insideFile[ptr]; !ok || !perInstance(files, owner) {
					add(ptr, "x-pack-compute reads %s: replicas are byte-identical, so an instance's identity may only reach a perInstance file", r)
				}
			}
			if c.IsLibrary && strings.HasPrefix(r, "peers.") {
				add(ptr, "a library reads peers (%s): only the consumer has neighbours", r)
			}
			// a neighbour's values are read through a role the component declares
			if tail, ok := strings.CutPrefix(r, "peers."); ok && !c.IsLibrary {
				role, rest, _ := strings.Cut(tail, ".")
				if rest == "values" || strings.HasPrefix(rest, "values.") {
					if _, declared := root.Peers[role]; !declared {
						add(ptr, "x-pack-compute reads %s: %q is not a role this component declares in x-pack.peers", r, role)
					}
				}
			}
			if strings.HasSuffix(r, "[]") && strings.HasPrefix(r, "values.") {
				add(ptr, "dynamic index in %s: the set of reads must be static", r)
			}
			// 3. no formula reads a password
			if vp, ok := strings.CutPrefix(r, "values."); ok {
				target := "/" + strings.ReplaceAll(vp, ".", "/")
				for _, pw := range passwordLeaves {
					if target == pw || strings.HasPrefix(pw, target+"/") {
						add(ptr, "x-pack-compute reads the password %s: a derived value is stored", pw)
					}
				}
			}
		}
	}
	if cycle := findCycle(reads); len(cycle) > 0 {
		add(cycle, "x-pack-compute reads form a cycle")
	}

	// 4. every password leaf lands in a file or an env
	envSet := map[string]bool{}
	for _, e := range envLeaves {
		envSet[e] = true
	}
	for _, pw := range passwordLeaves {
		if _, inFile := insideFile[pw]; !inFile && !envSet[pw] {
			add(pw, "a password with nowhere to go: put it under an x-pack-file subtree or on an x-pack-env leaf")
		}
	}

	// 5. -flagfile= in argv ↔ a flagfile file; a flag name never in both
	argv := c.argv(root)
	flagfiles := map[string]bool{}
	for _, f := range files {
		if f.file.Format == "flagfile" {
			flagfiles[f.file.Path] = true
		}
	}
	for _, a := range argv {
		if p, ok := strings.CutPrefix(a, "-flagfile="); ok {
			if !flagfiles[p] {
				add("", "argv names -flagfile=%s but no x-pack-file of format flagfile has that path", p)
			}
		}
		if p, ok := strings.CutPrefix(a, "--flagfile="); ok {
			if !flagfiles[p] {
				add("", "argv names --flagfile=%s but no x-pack-file of format flagfile has that path", p)
			}
		}
	}

	// 6. files: path form, node-owned dirs, format, template, parseAs, perInstance
	seenPaths := map[string]string{}
	for _, f := range files {
		fp := f.file
		if !path.IsAbs(fp.Path) || path.Clean(fp.Path) != fp.Path || strings.Contains(fp.Path, "..") {
			add(f.ptr, "x-pack-file.path %q must be absolute and clean", fp.Path)
		}
		for _, owned := range nodeOwned {
			if fp.Path == owned || strings.HasPrefix(fp.Path, owned+"/") {
				add(f.ptr, "x-pack-file.path %q is owned by the node: the file would silently lose", fp.Path)
			}
		}
		if prev, dup := seenPaths[fp.Path]; dup {
			add(f.ptr, "x-pack-file.path %q also declared at %s", fp.Path, prev)
		}
		seenPaths[fp.Path] = f.ptr
		if !slices.Contains(packschema.Formats, fp.Format) && fp.Format != packschema.FormatSecret {
			add(f.ptr, "x-pack-file.format %q is not an emitter (%s, %s)", fp.Format, strings.Join(packschema.Formats, ", "), packschema.FormatSecret)
		}
		if fp.Format == packschema.FormatRaw {
			if len(fp.Template) == 0 {
				add(f.ptr, "format raw needs a template name: the file it is")
			} else if _, ok := c.Templates[fp.Template]; !ok && c.Templates != nil {
				add(f.ptr, "template %q is not among the component's templates (%s)", fp.Template, strings.Join(keys(c.Templates), ", "))
			}
			if len(fp.ParseAs) > 0 {
				add(f.ptr, "parseAs only belongs to format template")
			}
		} else if fp.Format == "template" {
			if len(fp.Template) == 0 {
				add(f.ptr, "format template needs a template name")
			} else if _, ok := c.Templates[fp.Template]; !ok && c.Templates != nil {
				add(f.ptr, "template %q is not among the component's templates (%s)", fp.Template, strings.Join(keys(c.Templates), ", "))
			}
			if len(fp.ParseAs) == 0 {
				add(f.ptr, "format template must declare parseAs: a template is the one emitter that can produce garbage")
			} else if !slices.Contains(packschema.Formats, fp.ParseAs) || fp.ParseAs == "template" {
				add(f.ptr, "parseAs %q is not a parseable format", fp.ParseAs)
			}
		} else if len(fp.Template) > 0 || len(fp.ParseAs) > 0 {
			add(f.ptr, "template/parseAs only belong to format template")
		}
		if len(fp.Mode) > 0 && !validMode(fp.Mode) {
			add(f.ptr, "mode %q is not an octal file mode", fp.Mode)
		}
		if fp.PerInstance && scale != "replicas" {
			add(f.ptr, "perInstance needs scale: replicas")
		}
	}

	// 7. readiness names a port; scale is single|replicas; a library has neither
	if c.IsLibrary {
		for _, k := range []string{"platform", "ports"} {
			if _, has := c.Schema.Root.Properties[k]; has {
				add("/"+k, "a library declares %s: it has no workload of its own", k)
			}
		}
		if len(scale) > 0 {
			add("/x-pack/scale", "a library declares scale: it has no workload of its own")
		}
	} else {
		if n := c.Schema.Root.Properties["scale"]; n != nil && n.HasConst {
			add("/scale", "scale is x-pack.scale, a fact of the root — not a value; regenerate the schema")
		}
		switch scale {
		case "single", "replicas":
		case "":
			add("/x-pack/scale", "a component with a workload declares x-pack.scale: single or replicas")
		default:
			add("/x-pack/scale", "scale must be single or replicas, got %q", scale)
		}
		switch root.RestartPolicy {
		case "", "Always", "OnFailure":
		case "Never":
			if scale == "replicas" {
				add("/x-pack/restartPolicy", "a run-to-completion component runs once: scale replicas makes no sense")
			}
		default:
			add("/x-pack/restartPolicy", "restartPolicy must be Always, OnFailure or Never, got %q", root.RestartPolicy)
		}
		if root.Readiness != nil {
			if len(root.Readiness.TCP) > 0 && !ports[root.Readiness.TCP] {
				add("/x-pack/readiness", "tcp names port %q, which ports.* does not declare", root.Readiness.TCP)
			}
			if len(root.Readiness.HTTP) > 0 {
				p, _, _ := strings.Cut(root.Readiness.HTTP, "/")
				if !ports[p] {
					add("/x-pack/readiness", "http names port %q, which ports.* does not declare", p)
				}
			}
		}
	}

	// 8. requires ↔ pack:// refs both ways
	referenced := map[string]bool{}
	for name, n := range c.Schema.Root.Properties {
		if n.IsForeign() && strings.HasPrefix(n.Ref, packschema.PackScheme) {
			referenced[strings.TrimSuffix(strings.TrimPrefix(n.Ref, packschema.PackScheme), "#")] = true
		}
		_ = name
	}
	for lib, cons := range root.Requires {
		if !referenced[lib] {
			add("/x-pack/requires/"+lib, "declared but no key references pack://%s#", lib)
		}
		if _, err := semver.ParseConstraint(cons); err != nil {
			add("/x-pack/requires/"+lib, "%v", err)
		}
	}

	// 9. hooks and upgrade
	if root.Upgrade != nil {
		if _, err := semver.ParseConstraint(root.Upgrade.From); err != nil {
			add("/x-pack/upgrade/from", "%v", err)
		}
	}
	for i, h := range root.Hooks {
		ptr := fmt.Sprintf("/x-pack/hooks/%d", i)
		if len(h.Name) == 0 {
			add(ptr, "hook without a name")
		}
		if !slices.Contains(packschema.Phases, h.Phase) {
			add(ptr, "phase %q is not one of %s", h.Phase, strings.Join(packschema.Phases, ", "))
		}
		if len(h.Image) > 0 && h.Image != "new" && h.Image != "old" {
			add(ptr, "image must be new or old, got %q", h.Image)
		}
		if len(h.Command) == 0 {
			add(ptr, "hook without a command")
		}
		if h.When != nil {
			for _, cons := range []string{h.When.From, h.When.To} {
				if _, err := semver.ParseConstraint(cons); err != nil {
					add(ptr+"/when", "%v", err)
				}
			}
		}
		if h.Exclusive && c.IsLibrary {
			add(ptr, "exclusive hook on a library: nothing to stop")
		}
		if packschema.IsInstall(h.Phase) {
			if h.Exclusive {
				add(ptr, "exclusive on an install hook: the component is not running yet")
			}
			if h.When != nil && len(h.When.From) > 0 {
				add(ptr+"/when/from", "an install hook has no version to come from")
			}
			if h.Image == "old" {
				add(ptr+"/image", "an install hook has no old image")
			}
		}
		if h.Compensate != nil && len(h.Compensate.Command) == 0 {
			add(ptr+"/compensate", "compensate without a command")
		}
		if len(h.For) > 0 {
			add(ptr, "`for` belongs to a composition hook, not a component's")
		}
	}

	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Path < ps[j].Path })
	return ps, nil
}

// argv is the static part of the command line: the image's entrypoint+cmd, or
// x-pack.command in its place. Leaves annotated x-pack-arg join at render time.
func (c Component) argv(root *packschema.Root) []string {
	if len(root.Command) > 0 {
		return root.Command
	}
	if c.Image != nil {
		return append(append([]string{}, c.Image.Config.Entrypoint...), c.Image.Config.Cmd...)
	}
	return nil
}

// findCycle returns a pointer on a cycle among formulas that read each other
// through values.*, or "".
func findCycle(reads map[string][]string) string {
	const white, grey, black = 0, 1, 2
	color := map[string]int{}
	var visit func(ptr string) string
	visit = func(ptr string) string {
		switch color[ptr] {
		case grey:
			return ptr
		case black:
			return ""
		}
		color[ptr] = grey
		for _, r := range reads[ptr] {
			vp, ok := strings.CutPrefix(r, "values.")
			if !ok {
				continue
			}
			target := "/" + strings.ReplaceAll(vp, ".", "/")
			for other := range reads {
				if other == target || strings.HasPrefix(target, other+"/") {
					if c := visit(other); len(c) > 0 {
						return c
					}
				}
			}
		}
		color[ptr] = black
		return ""
	}
	ptrs := make([]string, 0, len(reads))
	for p := range reads {
		ptrs = append(ptrs, p)
	}
	sort.Strings(ptrs)
	for _, p := range ptrs {
		if c := visit(p); len(c) > 0 {
			return c
		}
	}
	return ""
}

// perInstance reports whether the file at ptr is declared perInstance.
func perInstance(files []fileAt, ptr string) bool {
	for _, f := range files {
		if f.ptr == ptr {
			return f.file.PerInstance
		}
	}
	return false
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func validMode(m string) bool {
	if len(m) != 4 && len(m) != 3 {
		return false
	}
	for _, r := range m {
		if r < '0' || r > '7' {
			return false
		}
	}
	return true
}

// typeAccepts says whether a node of the given JSON types takes a value of the
// formula's type: `number` takes an integer too, nothing else is coerced.
func typeAccepts(nodeTypes []string, got string) bool {
	for _, t := range nodeTypes {
		if t == got || (t == "number" && got == "integer") {
			return true
		}
	}
	return false
}
