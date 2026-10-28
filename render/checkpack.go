package render

import (
	"fmt"
	"strings"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/sdk/check"
	"github.com/ks-tool/pack/sdk/compute"
	"github.com/ks-tool/pack/sdk/packschema"
	"github.com/ks-tool/pack/sdk/schema"
)

type (
	schemaT    = schema.Schema
	schemaNode = schema.Node
)

// CheckBundle runs the author checks of every component in a bundle (spec
// §2.6) and the pack-level ones (§3.4) that need no values: membership, role
// binding, ordering, savepoints and hooks naming real keys.
func CheckBundle(b *bundle.Bundle) ([]check.Problem, error) {
	var problems []check.Problem
	for _, c := range b.Components {
		ps, err := check.Run(check.Component{Name: c.Name, Schema: c.Schema, Templates: c.Templates, Image: c.Image, IsLibrary: c.IsLibrary})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Name, err)
		}
		for _, p := range ps {
			problems = append(problems, check.Problem{Path: c.Name + ":" + p.Path, Message: p.Message})
		}
	}
	add := func(path, format string, args ...any) {
		problems = append(problems, check.Problem{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	// roles and order — the same derivation the renderer does, on an empty document
	r := &render{b: b, userSet: map[string]bool{}, peers: map[string]map[string]string{}, report: Report{Computed: map[string]any{}, Secrets: map[string][]string{}}}
	if err := r.configure(Values{}); err != nil {
		add("", "%v", err)
	} else if err := r.checkNeighbourReads(add); err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, k := range b.Keys {
		keys[k] = true
	}
	for _, sp := range b.Root.Savepoints {
		if !keys[sp] {
			add("/x-pack/savepoints", "%q is not a key of the composition", sp)
		}
	}
	for i, h := range b.Root.Hooks {
		for _, k := range h.For {
			if !keys[k] {
				add(fmt.Sprintf("/x-pack/hooks/%d/for", i), "%q is not a key of the composition", k)
			}
		}
		if len(h.For) == 0 {
			add(fmt.Sprintf("/x-pack/hooks/%d", i), "a composition hook names the keys it is for")
		}
		if len(h.Image) == 0 {
			add(fmt.Sprintf("/x-pack/hooks/%d/image", i), "a composition hook names the member whose image runs it")
		} else if m, ok := b.Components[h.Image]; !ok {
			add(fmt.Sprintf("/x-pack/hooks/%d/image", i), "%q is not a member of the pack", h.Image)
		} else if len(m.ImageRef) == 0 {
			add(fmt.Sprintf("/x-pack/hooks/%d/image", i), "member %q has no image", h.Image)
		}
	}
	// libraries: every requires satisfied by a member of a fitting version
	for _, key := range b.Keys {
		comp := b.Components[b.KeyComponent[key]]
		for lib, cons := range comp.Root.Requires {
			lc, ok := b.Components[lib]
			if !ok {
				add("/"+key, "requires library %q, which is not a member", lib)
				continue
			}
			if !versionFits(lc.Version, cons) {
				add("/"+key, "requires %s %s, the member is %s", lib, cons, lc.Version)
			}
		}
	}
	// files: one path, one owner across a component and its libraries
	for _, key := range b.Keys {
		owners := map[string]string{}
		err := b.Composition.Visit(func(ptr string, _ *schemaT, n *schemaNode) error {
			if !strings.HasPrefix(ptr, "/"+key+"/") || n.File == nil {
				return nil
			}
			if prev, dup := owners[n.File.Path]; dup {
				add(ptr, "x-pack-file.path %q is also written by %s", n.File.Path, prev)
			}
			owners[n.File.Path] = ptr
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	_ = packschema.PackScheme
	return problems, nil
}

// checkNeighbourReads: a formula reading peers.<role>.values must not reach a
// password leaf of the neighbour the role is bound to — the render takes the
// leaf out of what it shows, so the formula would see nothing and the author
// should hear of it. An unbound optional role is nothing to check.
func (r *render) checkNeighbourReads(add func(path, format string, args ...any)) error {
	env, err := compute.Env()
	if err != nil {
		return err
	}
	r.env = env
	if err := r.collect(); err != nil {
		add("", "%v", err)
		return nil
	}
	for _, f := range r.formulas {
		for _, rd := range f.reads {
			role, rest, ok := peerValuesRead(rd)
			if !ok || len(rest) == 0 {
				continue
			}
			peer, bound := r.peers[f.key][role]
			if !bound {
				continue
			}
			ptr := "/" + peer + "/" + strings.ReplaceAll(rest, ".", "/")
			if _, n, err := r.b.Composition.Lookup(ptr); err == nil && n != nil && n.IsPassword() {
				add(f.ptr, "x-pack-compute reads the password %s of its neighbour %s: a derived value is stored", ptr, peer)
			}
		}
	}
	return nil
}
