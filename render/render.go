package render

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	"cel.dev/cel-go/cel"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/sdk/compute"
	"github.com/ks-tool/pack/sdk/packschema"
	"github.com/ks-tool/pack/sdk/schema"
)

// Context is what the bundle cannot know: where and under what name it is
// being installed.
type Context struct {
	Installation string
	Namespace    string
}

// Values is the operator's document.
type Values struct {
	Values  map[string]any      `json:"values"`
	Rollout *packschema.Rollout `json:"rollout,omitempty"`
}

// formula is one x-pack-compute, located in the instance document.
type formula struct {
	ptr      string // instance pointer of the leaf
	root     string // pointer of the subtree `values` means for it (the key, or a library under it)
	key      string // system key it belongs to
	ast      *cel.Ast
	reads    []string
	instance bool // reads horchestra.instance/index/ordinal — evaluated per replica only
}

type render struct {
	b        *bundle.Bundle
	ctx      Context
	env      *cel.Env
	doc      map[string]any  // the effective document, whole
	userSet  map[string]bool // pointers the operator wrote
	formulas []formula
	peers    map[string]map[string]string // key → role → key
	order    []string
	report   Report
}

// Render runs the link: configure → mutate → validate → convert.
func Render(b *bundle.Bundle, v Values, ctx Context) (*Output, error) {
	if len(ctx.Installation) == 0 {
		return nil, errors.New("an installation name is required (--name)")
	}
	env, err := compute.Env()
	if err != nil {
		return nil, err
	}
	r := &render{b: b, ctx: ctx, env: env, userSet: map[string]bool{}, peers: map[string]map[string]string{},
		report: Report{Installation: ctx.Installation, Namespace: ctx.Namespace, Pack: b.Name, Computed: map[string]any{}, Secrets: map[string][]string{}}}
	if err := r.configure(v); err != nil {
		return nil, err
	}
	if err := r.mutate(); err != nil {
		return nil, err
	}
	if err := r.validate(); err != nil {
		return nil, err
	}
	return r.convert(v)
}

// configure: defaults ⊕ user values, peers bound, order derived.
func (r *render) configure(v Values) error {
	defaults, _, err := r.b.Composition.Defaults()
	if err != nil {
		return fmt.Errorf("defaults: %w", err)
	}
	doc, _ := defaults.(map[string]any)
	if doc == nil {
		doc = map[string]any{}
	}
	if v.Values != nil {
		pointers(v.Values, "", r.userSet)
		schema.Overlay(doc, schema.DeepCopy(v.Values).(map[string]any))
	}
	r.doc = doc
	for _, key := range r.b.Keys {
		if _, ok := doc[key]; !ok {
			doc[key] = map[string]any{}
		}
	}

	// peers: auto by uniqueness of type, explicit when ambiguous
	byType := map[string][]string{}
	for _, key := range r.b.Keys {
		byType[r.b.KeyComponent[key]] = append(byType[r.b.KeyComponent[key]], key)
	}
	for _, key := range r.b.Keys {
		comp := r.b.Components[r.b.KeyComponent[key]]
		ann, err := r.b.Composition.Root.Properties[key].KeyAnnotation()
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		bound := map[string]string{}
		for role, peer := range comp.Root.Peers {
			if explicit, ok := ann.Peers[role]; ok {
				if r.b.KeyComponent[explicit] != peer.Schema {
					return fmt.Errorf("%s: role %q bound to %q, which is %s, not %s", key, role, explicit, r.b.KeyComponent[explicit], peer.Schema)
				}
				bound[role] = explicit
				continue
			}
			candidates := byType[peer.Schema]
			switch {
			case len(candidates) == 1:
				bound[role] = candidates[0]
			case len(candidates) == 0 && peer.Required:
				return fmt.Errorf("%s: role %q needs a %s and the composition has none", key, role, peer.Schema)
			case len(candidates) > 1:
				return fmt.Errorf("%s: role %q is ambiguous — %s are all %s; bind it in x-pack.peers of the key", key, role, strings.Join(candidates, ", "), peer.Schema)
			}
		}
		for role := range ann.Peers {
			if _, known := comp.Root.Peers[role]; !known {
				return fmt.Errorf("%s: x-pack.peers names role %q, which %s does not declare", key, role, comp.Name)
			}
		}
		r.peers[key] = bound
	}
	r.report.Peers = r.peers

	// order: after every bound peer, plus explicit after
	edges := map[string][]string{}
	for _, key := range r.b.Keys {
		ann, _ := r.b.Composition.Root.Properties[key].KeyAnnotation()
		for _, peer := range r.peers[key] {
			edges[key] = append(edges[key], peer)
		}
		for _, a := range ann.After {
			if _, ok := r.b.KeyComponent[a]; !ok {
				return fmt.Errorf("%s: after names %q, not a key", key, a)
			}
			edges[key] = append(edges[key], a)
		}
	}
	order, err := topo(r.b.Keys, edges)
	if err != nil {
		return err
	}
	r.order = order
	r.report.Order = order
	return nil
}

// topo sorts keys so that every key comes after the keys it depends on,
// keeping the composition's order among independent ones.
func topo(keys []string, deps map[string][]string) ([]string, error) {
	const white, grey, black = 0, 1, 2
	color := map[string]int{}
	var out []string
	var visit func(k string, trail []string) error
	visit = func(k string, trail []string) error {
		switch color[k] {
		case grey:
			return fmt.Errorf("ordering cycle: %s → %s", strings.Join(trail, " → "), k)
		case black:
			return nil
		}
		color[k] = grey
		for _, d := range deps[k] {
			if err := visit(d, append(trail, k)); err != nil {
				return err
			}
		}
		color[k] = black
		out = append(out, k)
		return nil
	}
	for _, k := range keys {
		if err := visit(k, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// collect gathers every formula of the composition with its values root.
func (r *render) collect() error {
	roots := map[string]bool{}
	for _, key := range r.b.Keys {
		roots["/"+key] = true
	}
	return r.b.Composition.Visit(func(ptr string, s *schema.Schema, n *schema.Node) error {
		segs := schema.Split(ptr)
		if len(segs) == 0 || segs[0] == "common" {
			return nil
		}
		if n.IsForeign() && strings.HasPrefix(n.Ref, packschema.PackScheme) && len(segs) >= 2 {
			roots[ptr] = true
		}
		if len(n.Compute) == 0 {
			return nil
		}
		a, err := compute.Compile(r.env, n.Compute)
		if err != nil {
			return fmt.Errorf("%s: x-pack-compute: %w", ptr, err)
		}
		f := formula{ptr: ptr, key: segs[0], ast: a, reads: compute.Reads(a)}
		f.root = "/" + segs[0]
		for root := range roots {
			if strings.HasPrefix(ptr, root+"/") && len(root) > len(f.root) {
				f.root = root
			}
		}
		for _, rd := range f.reads {
			if rd == "horchestra.instance" || rd == "horchestra.index" || rd == "horchestra.ordinal" {
				f.instance = true
			}
		}
		r.formulas = append(r.formulas, f)
		return nil
	})
}

// mutate evaluates formulas — those a replica does not change — in an order
// where every values.* read is settled first; a leaf the operator wrote is
// left alone.
func (r *render) mutate() error {
	if err := r.collect(); err != nil {
		return err
	}
	pending := map[string]*formula{}
	for i := range r.formulas {
		f := &r.formulas[i]
		if f.instance || r.userSet[f.ptr] || userWroteAbove(r.userSet, f.ptr) {
			continue
		}
		pending[f.ptr] = f
	}
	for len(pending) > 0 {
		progressed := false
		keys := make([]string, 0, len(pending))
		for k := range pending {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, ptr := range keys {
			f := pending[ptr]
			if r.blocked(f, pending) {
				continue
			}
			v, err := r.eval(f, r.doc, nil)
			if err != nil {
				return fmt.Errorf("%s: x-pack-compute %q: %w", f.ptr, f.ast.Source().Content(), err)
			}
			if v != nil {
				if err := set(r.doc, f.ptr, v); err != nil {
					return err
				}
				r.report.Computed[f.ptr] = v
			}
			delete(pending, ptr)
			progressed = true
		}
		if !progressed {
			left := make([]string, 0, len(pending))
			for k := range pending {
				left = append(left, k)
			}
			sort.Strings(left)
			return fmt.Errorf("x-pack-compute: a cycle among %s", strings.Join(left, ", "))
		}
	}
	return nil
}

// userWroteAbove: the operator set an ancestor as a whole (an object literal
// replacing the subtree) — leave the subtree as written.
func userWroteAbove(userSet map[string]bool, ptr string) bool {
	for p := range userSet {
		if len(p) > 0 && strings.HasPrefix(ptr, p+"/") {
			return true
		}
	}
	return false
}

// blocked reports whether f reads a leaf whose formula is still pending.
func (r *render) blocked(f *formula, pending map[string]*formula) bool {
	for _, rd := range f.reads {
		rel, ok := strings.CutPrefix(rd, "values.")
		if !ok {
			continue
		}
		target := f.root + "/" + strings.ReplaceAll(rel, ".", "/")
		for p := range pending {
			if p == f.ptr {
				continue
			}
			if p == target || strings.HasPrefix(p, target+"/") || strings.HasPrefix(target, p+"/") {
				return true
			}
		}
	}
	return false
}

// eval runs one formula against a document; instance, when non-nil, adds the
// per-replica fields to the horchestra context.
func (r *render) eval(f *formula, doc map[string]any, instance map[string]any) (any, error) {
	sub, _ := get(doc, f.root)
	keyDoc, _ := get(doc, "/"+f.key)
	keyMap, _ := keyDoc.(map[string]any)
	if keyMap == nil {
		keyMap = map[string]any{}
	}
	platform, _ := keyMap["platform"].(map[string]any)
	common, _ := doc["common"].(map[string]any)
	vars := map[string]any{
		"values":     orEmpty(sub),
		"platform":   orEmpty(platform),
		"common":     orEmpty(common),
		"me":         r.me(f.key),
		"peers":      r.peerVars(f.key),
		"horchestra": r.horchestra(f.key, instance),
	}
	return compute.Eval(r.env, f.ast, vars)
}

func orEmpty(m any) any {
	if m == nil {
		return map[string]any{}
	}
	if mm, ok := m.(map[string]any); ok && mm == nil {
		return map[string]any{}
	}
	return m
}

func (r *render) service(key string) string { return r.ctx.Installation + "-" + key }

func (r *render) replicas(key string) int {
	keyDoc, _ := get(r.doc, "/"+key)
	km, _ := keyDoc.(map[string]any)
	if scale, _ := km["scale"].(string); scale != "replicas" {
		return 0
	}
	if p, ok := km["platform"].(map[string]any); ok {
		switch n := p["replicas"].(type) {
		case float64:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return int(i)
			}
		}
	}
	return 1
}

// me describes a key by names only: service, instances, replicas, ports.
func (r *render) me(key string) map[string]any {
	svc := r.service(key)
	n := r.replicas(key)
	var instances []any
	if n == 0 {
		instances = []any{svc + "." + r.ctx.Namespace}
	} else {
		for i := range n {
			instances = append(instances, fmt.Sprintf("%s-%d.%s.%s", svc, i, svc, r.ctx.Namespace))
		}
	}
	ports := map[string]any{}
	keyDoc, _ := get(r.doc, "/"+key)
	if km, ok := keyDoc.(map[string]any); ok {
		if pm, ok := km["ports"].(map[string]any); ok {
			maps.Copy(ports, pm)
		}
	}
	return map[string]any{"service": svc, "instances": instances, "replicas": max(n, 1), "ports": ports}
}

func (r *render) peerVars(key string) map[string]any {
	out := map[string]any{}
	for role, peer := range r.peers[key] {
		out[role] = r.me(peer)
	}
	return out
}

func (r *render) horchestra(key string, instance map[string]any) map[string]any {
	h := map[string]any{
		"namespace":    r.ctx.Namespace,
		"installation": r.ctx.Installation,
		"component":    key,
		"service":      r.service(key),
		"replicas":     max(r.replicas(key), 1),
	}
	maps.Copy(h, instance)
	return h
}

// instanceDoc re-evaluates the per-replica formulas of a key for replica i,
// returning a copy of the key's subtree with them applied.
func (r *render) instanceDoc(key string, i int) (map[string]any, error) {
	copyDoc := schema.DeepCopy(r.doc).(map[string]any)
	instance := map[string]any{"instance": fmt.Sprintf("%s-%d", r.service(key), i), "index": i, "ordinal": i + 1}
	for idx := range r.formulas {
		f := &r.formulas[idx]
		if !f.instance || f.key != key || r.userSet[f.ptr] {
			continue
		}
		v, err := r.eval(f, copyDoc, instance)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.ptr, err)
		}
		if v != nil {
			if err := set(copyDoc, f.ptr, v); err != nil {
				return nil, err
			}
		}
	}
	sub, _ := get(copyDoc, "/"+key)
	m, _ := sub.(map[string]any)
	return m, nil
}

// validate: the effective document against the composition's validating view,
// then the sizes the schema cannot express.
func (r *render) validate() error {
	// Under its own URL, not pack://<name>: a lone component's composition is
	// named after the component, and the ref pack://<name># must reach the
	// component, not the composition wrapped around it.
	compiled, err := r.b.Composition.Compile("horchestra://composition/" + r.b.Name)
	if err != nil {
		return err
	}
	var problems []string
	for _, v := range compiled.Validate(r.doc) {
		problems = append(problems, v.String())
	}
	for _, key := range r.b.Keys {
		ann, _ := r.b.Composition.Root.Properties[key].KeyAnnotation()
		keyDoc, _ := get(r.doc, "/"+key)
		km, _ := keyDoc.(map[string]any)
		req := map[string]string{}
		if p, ok := km["platform"].(map[string]any); ok {
			if res, ok := p["resources"].(map[string]any); ok {
				if rq, ok := res["requests"].(map[string]any); ok {
					for k, v := range rq {
						req[k], _ = v.(string)
					}
				}
			}
		}
		for res, min := range ann.Minimum {
			have, ok := req[res]
			if !ok {
				problems = append(problems, fmt.Sprintf("/%s/platform/resources/requests/%s: the composition requires at least %s", key, res, min))
				continue
			}
			hv, err1 := compute.Quantity(have)
			mv, err2 := compute.Quantity(min)
			if err1 == nil && err2 == nil && hv < mv {
				problems = append(problems, fmt.Sprintf("/%s/platform/resources/requests/%s: %s is below the composition's minimum %s", key, res, have, min))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("values do not validate:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}
