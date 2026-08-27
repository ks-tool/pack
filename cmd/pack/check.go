package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ks-tool/pack/registry"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/sdk/check"
	"github.com/ks-tool/pack/sdk/schema"
)

func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	plainHTTP := fs.Bool("plain-http", false, "the registry speaks plain HTTP")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("check: one argument — a component or composition directory, or a registry reference")
	}
	dir := fs.Arg(0)
	var problems []check.Problem
	if registry.IsRef(dir) || isComposition(dir) {
		b, err := openPack(dir, *plainHTTP)
		if err != nil {
			return err
		}
		if problems, err = render.CheckBundle(b); err != nil {
			return err
		}
	} else {
		comps, err := componentsFromDir(dir)
		if err != nil {
			return err
		}
		for _, c := range comps {
			ps, err := check.Run(c)
			if err != nil {
				return fmt.Errorf("%s: %w", c.Name, err)
			}
			problems = append(problems, ps...)
		}
	}
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%d problem(s)", len(problems))
	}
	fmt.Println("ok")
	return nil
}

// isComposition: a schema whose keys are all pack:// references and which
// has none of a component's own keys.
func isComposition(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "schema.json"))
	if err != nil {
		return false
	}
	s, err := schema.Parse(raw)
	if err != nil {
		return false
	}
	refs := 0
	for name, n := range s.Root.Properties {
		if name == "common" {
			continue
		}
		if n.IsForeign() && strings.HasPrefix(n.Ref, "pack://") {
			refs++
		} else if name == "platform" || name == "ports" || name == "scale" || name == "claims" || n.File != nil {
			return false
		}
	}
	return refs > 0
}

// componentsFromDir reads an author's directory: schema.json and, if present,
// templates/*. There is no image here, so argv checks fall back to x-pack.command.
func componentsFromDir(dir string) ([]check.Component, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "schema.json"))
	if err != nil {
		return nil, err
	}
	s, err := schema.Parse(raw)
	if err != nil {
		return nil, err
	}
	templates := map[string][]byte{}
	entries, _ := os.ReadDir(filepath.Join(dir, "templates"))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, "templates", e.Name()))
		if err != nil {
			return nil, err
		}
		templates[e.Name()] = b
	}
	if err := attachLibraries(s, filepath.Dir(dir)); err != nil {
		return nil, err
	}
	if err := s.AttachBuiltins(); err != nil {
		return nil, err
	}
	_, hasPlatform := s.Root.Properties["platform"]
	return []check.Component{{Name: filepath.Base(dir), Schema: s, Templates: templates, Image: imageConfig(dir), IsLibrary: !hasPlatform}}, nil
}

// imageConfig reads image.json beside the schema when the author put it there
// (`docker image inspect` / oci-packer give it): the entrypoint and cmd the
// argv checks want. Absent, the checks fall back to x-pack.command.
func imageConfig(dir string) *ocispec.Image {
	raw, err := os.ReadFile(filepath.Join(dir, "image.json"))
	if err != nil {
		return nil
	}
	var img ocispec.Image
	if json.Unmarshal(raw, &img) != nil {
		return nil
	}
	return &img
}

// attachLibraries resolves pack://<name># to a sibling directory <name>/schema.json
// when there is one, and to an open schema otherwise — a lone component is
// checkable without its libraries, a composition directory carries them.
func attachLibraries(s *schema.Schema, siblings string) error {
	for _, n := range s.Root.Properties {
		if !n.IsForeign() || !strings.HasPrefix(n.Ref, "pack://") {
			continue
		}
		url, _ := schema.SplitRef(n.Ref)
		name := strings.TrimPrefix(url, "pack://")
		raw, err := os.ReadFile(filepath.Join(siblings, name, "schema.json"))
		if err != nil {
			open, _ := schema.Parse([]byte(`{"type":"object"}`))
			s.Attach(url, open)
			continue
		}
		ls, err := schema.Parse(raw)
		if err != nil {
			return fmt.Errorf("library %s: %w", name, err)
		}
		ls.Foreign = s.Foreign
		s.Attach(url, ls)
	}
	return nil
}
