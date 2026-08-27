// Package bundle reads a composition, or a lone component, from a Source: a
// directory tree (the author's working tree — the same files oci-packer
// packs; a composition directory holds schema.json, each key's component is
// the sibling directory named by pack://<name>#, with schema.json,
// templates/, optionally image.json and pack.yaml) or a registry (the pack
// as oci-packer pushed it: the index, the schema and template blobs, the
// image configs — never a layer).
package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	packer "github.com/arenadata/oci-packer/types"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/registry"
	"github.com/ks-tool/pack/sdk/packschema"
	"github.com/ks-tool/pack/sdk/schema"
)

type Component struct {
	Name, Version string
	Dir           string
	Schema        *schema.Schema
	SchemaRaw     []byte
	Root          *packschema.Root
	Templates     map[string][]byte
	Image         *ocispec.Image // nil when image.json is absent
	ImageRef      string         // registry reference of the image, from pack.yaml or image.ref
	IsLibrary     bool
}

type Bundle struct {
	Name, Version string
	Dir           string
	Composition   *schema.Schema
	Root          *packschema.Root
	Keys          []string          // the system's keys, in the composition's order (common excluded)
	KeyComponent  map[string]string // key → component name
	Components    map[string]*Component
}

// Load reads dir as a composition, or as a lone component wrapped in a
// composition of one key.
func Load(dir string) (*Bundle, error) {
	b, err := LoadFrom(Dir(dir))
	if err != nil {
		return nil, err
	}
	b.Dir = dir
	return b, nil
}

// LoadRef reads a pack, or a lone component, straight from a registry: the
// index, the schemas, the templates and the image configs — no layer.
func LoadRef(ctx context.Context, c *registry.Client, ref string) (*Bundle, error) {
	r, err := registry.Parse(ref)
	if err != nil {
		return nil, err
	}
	b, err := LoadFrom(Registry(ctx, c, r))
	if err != nil {
		return nil, err
	}
	b.Dir = r.String()
	return b, nil
}

// LoadFrom builds a bundle from a source: the root is a composition, or a
// component wrapped in a composition of one key.
func LoadFrom(src Source) (*Bundle, error) {
	root, err := src.Root()
	if err != nil {
		return nil, err
	}
	s, err := schema.Parse(root.Schema)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", root.Origin, err)
	}
	if isComponent(s) {
		name := root.Name
		comp := fmt.Sprintf(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"properties":{%q:{"$ref":"pack://%s#"}}}`, name, name)
		if s, err = schema.Parse([]byte(comp)); err != nil {
			return nil, err
		}
		return load(selfOrMember{src, root}, name, s)
	}
	b, err := load(src, root.Name, s)
	if err != nil {
		return nil, err
	}
	b.Version = root.Version
	return b, nil
}

// selfOrMember answers the lone component's own name with its own files.
type selfOrMember struct {
	Source
	root *Files
}

func (s selfOrMember) Member(name string) (*Files, error) {
	if name == s.root.Name {
		return s.root, nil
	}
	return s.Source.Member(name)
}

func isComponent(s *schema.Schema) bool {
	for _, k := range []string{"platform", "ports", "scale", "claims"} {
		if _, ok := s.Root.Properties[k]; ok {
			return true
		}
	}
	for _, n := range s.Root.Properties {
		if n.File != nil || len(n.Compute) > 0 {
			return true
		}
	}
	return false
}

func load(src Source, name string, comp *schema.Schema) (*Bundle, error) {
	b := &Bundle{Name: name, Composition: comp, KeyComponent: map[string]string{}, Components: map[string]*Component{}}
	if err := comp.AttachBuiltins(); err != nil {
		return nil, err
	}
	root, err := comp.Root.RootAnnotation()
	if err != nil {
		return nil, fmt.Errorf("composition: %w", err)
	}
	b.Root = root
	for _, key := range comp.Root.Order {
		n := comp.Root.Properties[key]
		if key == "common" {
			continue
		}
		if !n.IsForeign() || !strings.HasPrefix(n.Ref, packschema.PackScheme) {
			return nil, fmt.Errorf("composition key %q: not a pack://<name># reference", key)
		}
		url, _ := schema.SplitRef(n.Ref)
		cname := strings.TrimPrefix(url, packschema.PackScheme)
		if _, err := b.component(src, cname, comp); err != nil {
			return nil, err
		}
		b.Keys = append(b.Keys, key)
		b.KeyComponent[key] = cname
	}
	if len(b.Keys) == 0 {
		return nil, errors.New("composition: no keys")
	}
	return b, nil
}

// component loads a component once, attaching its libraries.
func (b *Bundle) component(src Source, name string, comp *schema.Schema) (*Component, error) {
	if c, ok := b.Components[name]; ok {
		return c, nil
	}
	f, err := src.Member(name)
	if err != nil {
		return nil, fmt.Errorf("component %q: %w", name, err)
	}
	s, err := schema.Parse(f.Schema)
	if err != nil {
		return nil, fmt.Errorf("component %q: %w", name, err)
	}
	s.Foreign = comp.Foreign // one table for the whole bundle
	comp.Attach(packschema.PackScheme+name, s)
	root, err := s.Root.RootAnnotation()
	if err != nil {
		return nil, fmt.Errorf("component %q: %w", name, err)
	}
	c := &Component{Name: name, Version: f.Version, Dir: f.Origin, Schema: s, SchemaRaw: f.Schema, Root: root, Templates: f.Templates, ImageRef: f.ImageRef}
	if c.Templates == nil {
		c.Templates = map[string][]byte{}
	}
	b.Components[name] = c
	_, c.IsLibrary = s.Root.Properties["platform"]
	c.IsLibrary = !c.IsLibrary
	if f.ImageConfig != nil {
		var img ocispec.Image
		if err := json.Unmarshal(f.ImageConfig, &img); err != nil {
			return nil, fmt.Errorf("component %q: image config: %w", name, err)
		}
		c.Image = &img
	}
	for _, n := range s.Root.Properties {
		if n.IsForeign() && strings.HasPrefix(n.Ref, packschema.PackScheme) {
			url, _ := schema.SplitRef(n.Ref)
			lib := strings.TrimPrefix(url, packschema.PackScheme)
			if _, err := b.component(src, lib, comp); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

// readPackFile reads the directory's pack file — oci-packer's own type.
func readPackFile(dir string) *packer.Pack {
	for _, name := range []string{"pack.yaml", filepath.Base(dir) + ".pack.yaml"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var pf packer.Pack
		if yaml.Unmarshal(raw, &pf) == nil {
			return &pf
		}
	}
	return nil
}

func packFileIdentity(dir string) (name, version string) {
	if pf := readPackFile(dir); pf != nil {
		return pf.Annotations["io.horchestra.name"], pf.Annotations["io.horchestra.version"]
	}
	return "", ""
}

// imageRef is the image the component runs: the oci:// or cr:// item of its
// pack file, or the content of image.ref.
func imageRef(dir string) string {
	if pf := readPackFile(dir); pf != nil {
		for _, it := range pf.Items {
			for _, scheme := range []string{"cr://", "oci://", "docker://"} {
				if ref, ok := strings.CutPrefix(it.From, scheme); ok {
					return ref
				}
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "image.ref")); err == nil {
		return strings.TrimSpace(string(raw))
	}
	return ""
}
