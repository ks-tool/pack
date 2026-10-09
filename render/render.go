package render

import (
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

// Context is what the bundle cannot know: under what name it is being
// installed, and the target it is installed on.
type Context struct {
	Installation string
	Target       Target
}

// Target is what the render asks of the system it renders for. A key's
// platform subtree is the target's: its schema, how many instances it means,
// what it says about limits, whether it meets the composition's minimum —
// the render reads none of it itself.
type Target interface {
	Names
	// PlatformSchema is the schema of a key's platform subtree in the
	// target's words; its $id is platform.SlotID.
	PlatformSchema() []byte
	// Replicas is how many instances a key of scale: replicas runs.
	Replicas(key string, platform map[string]any) (int, error)
	// Limits are the facts formulas read as `limits`: memory in bytes, cpu
	// in millicores, absent where the target has no limit to tell.
	Limits(key string, platform map[string]any) (map[string]any, error)
	// ValidatePlatform checks a key's platform against what the composition
	// requires of it (x-pack.minimum), one "pointer: message" per problem.
	ValidatePlatform(key string, platform map[string]any, minimum map[string]string) []string
}

// Names is the one part of a target the render sees: how it names and
// reaches what it runs. Formulas read it as me and peers (service,
// instances) and as horchestra.service, horchestra.instance and
// horchestra.namespace — a pack is rendered once per target, never for
// "any" target, because a neighbour's address is part of a config file.
type Names interface {
	// Service is the stable name of a key as a whole.
	Service(key string) string
	// Instance is the name of replica i of a key.
	Instance(key string, i int) string
	// Address is how a neighbour reaches replica i of a key; i < 0 is the key
	// as one service, which is all a key that does not scale has.
	Address(key string, i int) string
	// Namespace is the scope the target installs into; empty where it has none.
	Namespace() string
}

// Values is the operator's document.
type Values struct {
	Values  map[string]any      `json:"values"`
	Rollout *packschema.Rollout `json:"rollout,omitempty"`
	// Skip names keys to exclude from whatever the selection would otherwise
	// install — the explicit values an operator named, or the whole-bundle
	// fallback. It is a final override, checked AFTER the selection and its
	// closure settle: a key still required by another key that IS installed
	// cannot be skipped, and configure refuses rather than installing a
	// component with a required peer quietly missing.
	Skip []string `json:"skip,omitempty"`
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
	// keys is the SELECTION this render actually installs — b.Keys narrowed to
	// what the operator named in values plus the required peers that follow
	// from it, in the composition's declared order. Everything below configure
	// works off this, never off b.Keys, once it is set.
	keys   []string
	order  []string
	report Report
	// instances and limits are the target's answers about each selected key,
	// asked once the platform subtrees are settled (resolve).
	instances map[string]int
	limits    map[string]map[string]any
}

// Render runs the link: configure → mutate → validate → convert, and stops at
// the target-neutral Release a target turns into its own objects.
func Render(b *bundle.Bundle, v Values, ctx Context) (*Release, error) {
	if len(ctx.Installation) == 0 {
		return nil, errors.New("an installation name is required (--name)")
	}
	if ctx.Target == nil {
		return nil, errors.New("a target is required: the render cannot size or address a key without one")
	}
	env, err := compute.Env()
	if err != nil {
		return nil, err
	}
	r := &render{b: b, ctx: ctx, env: env, userSet: map[string]bool{}, peers: map[string]map[string]string{},
		report: Report{Installation: ctx.Installation, Namespace: ctx.Target.Namespace(), Pack: b.Name, Computed: map[string]any{}}}
	if err := r.configure(v); err != nil {
		return nil, err
	}
	if err := r.resolve(); err != nil {
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

// configure: defaults ⊕ user values, selection and peers bound, order derived.
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

	// Selection: a key installs when the values document NAMES it, even as {}
	// — presence is the ask, not the content. Naming none of the composition's
	// keys at all is "no choice made", not "choose nothing": it keeps every
	// key, the way a values document always has, so a lone-component pack
	// installed with values: {} still installs its one component and an
	// operator who has never thought about subsetting sees no change.
	selected := map[string]bool{}
	if v.Values != nil {
		for k := range v.Values {
			if _, ok := r.b.KeyComponent[k]; ok {
				selected[k] = true
			}
		}
	}
	if len(selected) == 0 {
		for _, key := range r.b.Keys {
			selected[key] = true
		}
	}

	// byType is looked up against the WHOLE bundle, never the selection: a
	// required peer has to be IDENTIFIED before it can be ADDED to the
	// selection, which is what the closure below does.
	byType := map[string][]string{}
	for _, key := range r.b.Keys {
		byType[r.b.KeyComponent[key]] = append(byType[r.b.KeyComponent[key]], key)
	}

	// bindOne resolves one key's peers against byType — auto by uniqueness of
	// type, explicit in x-pack.peers of the key when ambiguous — unchanged
	// from before the closure existed: an unresolved REQUIRED role is still a
	// refusal here, because that is the pack being incomplete, not a question
	// of what this render selected.
	bindOne := func(key string) (map[string]string, error) {
		comp := r.b.Components[r.b.KeyComponent[key]]
		ann, err := r.b.Composition.Root.Properties[key].KeyAnnotation()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		bound := map[string]string{}
		for role, peer := range comp.Root.Peers {
			if explicit, ok := ann.Peers[role]; ok {
				if r.b.KeyComponent[explicit] != peer.Schema {
					return nil, fmt.Errorf("%s: role %q bound to %q, which is %s, not %s", key, role, explicit, r.b.KeyComponent[explicit], peer.Schema)
				}
				bound[role] = explicit
				continue
			}
			candidates := byType[peer.Schema]
			switch {
			case len(candidates) == 1:
				bound[role] = candidates[0]
			case len(candidates) == 0 && peer.Required:
				return nil, fmt.Errorf("%s: role %q needs a %s and the composition has none", key, role, peer.Schema)
			case len(candidates) > 1:
				return nil, fmt.Errorf("%s: role %q is ambiguous — %s are all %s; bind it in x-pack.peers of the key", key, role, strings.Join(candidates, ", "), peer.Schema)
			}
		}
		for role := range ann.Peers {
			if _, known := comp.Root.Peers[role]; !known {
				return nil, fmt.Errorf("%s: x-pack.peers names role %q, which %s does not declare", key, role, comp.Name)
			}
		}
		return bound, nil
	}

	// Closure: a REQUIRED peer's resolved key joins the selection, and joining
	// may itself have required peers, so this runs to a fixed point. A
	// required: false peer is bound below, once the selection is settled, but
	// never grows it here — "works without it" means without, not "pulls it
	// in anyway because one happens to exist".
	bound := map[string]map[string]string{}
	for {
		grew := false
		for key := range selected {
			if _, done := bound[key]; done {
				continue
			}
			b, err := bindOne(key)
			if err != nil {
				return err
			}
			bound[key] = b
			comp := r.b.Components[r.b.KeyComponent[key]]
			for role, peer := range comp.Root.Peers {
				if !peer.Required {
					continue
				}
				if target, ok := bound[key][role]; ok && !selected[target] {
					selected[target] = true
					grew = true
				}
			}
			// A one-shot init step (restartPolicy: Never) named in `after` is a
			// load-bearing prerequisite, not an ordering nicety: postgres without
			// pg-init just fails. It has no peer role to be required through — a
			// step that runs once and is gone is nothing a formula could address
			// — so `after` is the only way a component can name one, and here it
			// is mandatory, not "orders it, if it happens to already be selected".
			ann, err := r.b.Composition.Root.Properties[key].KeyAnnotation()
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			for _, a := range ann.After {
				if _, ok := r.b.KeyComponent[a]; !ok {
					return fmt.Errorf("%s: after names %q, not a key", key, a)
				}
				if selected[a] {
					continue
				}
				if r.b.Components[r.b.KeyComponent[a]].Root.RestartPolicy == "Never" {
					selected[a] = true
					grew = true
				}
			}
		}
		if !grew {
			break
		}
	}

	// Skip: an explicit override, checked against the closure it just
	// settled — not against the pre-closure selection, so skipping BOTH a key
	// and its only dependent in the same call is not a contradiction.
	for _, key := range v.Skip {
		if _, ok := r.b.KeyComponent[key]; !ok {
			return fmt.Errorf("--skip names %q, not a key", key)
		}
		delete(selected, key)
	}
	if len(v.Skip) > 0 {
		var broken []string
		for key := range selected {
			comp := r.b.Components[r.b.KeyComponent[key]]
			for role, peer := range comp.Root.Peers {
				if !peer.Required {
					continue
				}
				target, ok := bound[key][role]
				if ok && !selected[target] {
					broken = append(broken, fmt.Sprintf("%s requires %s (role %q)", key, target, role))
				}
			}
		}
		if len(broken) > 0 {
			sort.Strings(broken)
			return fmt.Errorf("cannot skip: %s", strings.Join(broken, "; "))
		}
	}
	r.keys = make([]string, 0, len(selected))
	for _, key := range r.b.Keys { // the composition's own declared order
		if selected[key] {
			r.keys = append(r.keys, key)
		}
	}

	// A required: false role wires up only when its candidate is ALSO
	// selected: an optional peer present by the operator's own choice, not one
	// pulled in for the occasion because bindOne could still name it.
	r.peers = map[string]map[string]string{}
	for _, key := range r.keys {
		out := map[string]string{}
		for role, target := range bound[key] {
			if selected[target] {
				out[role] = target
			}
		}
		r.peers[key] = out
	}
	r.report.Peers = r.peers

	// A key that was not selected gets no doc entry: nothing to default,
	// mutate, validate or render for a component this release does not
	// install. The composition schema has no top-level `required`, precisely
	// so that removing one here is not a validation failure later.
	for key := range doc {
		if _, isKey := r.b.KeyComponent[key]; isKey && !selected[key] {
			delete(doc, key)
		}
	}
	for _, key := range r.keys {
		if _, ok := doc[key]; !ok {
			doc[key] = map[string]any{}
		}
	}

	// order: after every bound peer, plus explicit after — both restricted to
	// the selection. `after` naming a key this release does not install is
	// dropped rather than refused: it is an ordering hint about a key that
	// simply is not here, not a broken reference the composition got wrong.
	edges := map[string][]string{}
	for _, key := range r.keys {
		ann, _ := r.b.Composition.Root.Properties[key].KeyAnnotation()
		for _, peer := range r.peers[key] {
			edges[key] = append(edges[key], peer)
		}
		for _, a := range ann.After {
			if _, ok := r.b.KeyComponent[a]; !ok {
				return fmt.Errorf("%s: after names %q, not a key", key, a)
			}
			if selected[a] {
				edges[key] = append(edges[key], a)
			}
		}
	}
	order, err := topo(r.keys, edges)
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

// collect gathers every formula of a SELECTED key with its values root — a
// key this release does not install has no doc entry (configure already
// deleted it), so evaluating its formulas would compute against nothing and
// set() into a path that no longer exists.
func (r *render) collect() error {
	selected := map[string]bool{}
	roots := map[string]bool{}
	for _, key := range r.keys {
		selected[key] = true
		roots["/"+key] = true
	}
	return r.b.Composition.Visit(func(ptr string, s *schema.Schema, n *schema.Node) error {
		segs := schema.Split(ptr)
		if len(segs) == 0 || segs[0] == "common" || !selected[segs[0]] {
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
	common, _ := doc["common"].(map[string]any)
	vars := map[string]any{
		"values":     orEmpty(sub),
		"limits":     orEmpty(r.limits[f.key]),
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

// resolve asks the target what each selected key's platform means: how many
// instances (scale: replicas only — single is one, addressed as the key) and
// the limits formulas may read.
func (r *render) resolve() error {
	r.instances, r.limits = map[string]int{}, map[string]map[string]any{}
	for _, key := range r.keys {
		keyDoc, _ := get(r.doc, "/"+key)
		km, _ := keyDoc.(map[string]any)
		platform, _ := km["platform"].(map[string]any)
		if r.b.Components[r.b.KeyComponent[key]].Root.Scale == "replicas" {
			n, err := r.ctx.Target.Replicas(key, platform)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			r.instances[key] = n
		}
		l, err := r.ctx.Target.Limits(key, platform)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		r.limits[key] = l
	}
	return nil
}

// replicas is 0 for a key that does not scale, n ≥ 1 for scale: replicas.
func (r *render) replicas(key string) int { return r.instances[key] }

// me describes a key by names only: service, instances, replicas, ports.
func (r *render) me(key string) map[string]any {
	svc := r.ctx.Target.Service(key)
	n := r.replicas(key)
	var instances []any
	if n == 0 {
		instances = []any{r.ctx.Target.Address(key, -1)}
	} else {
		for i := range n {
			instances = append(instances, r.ctx.Target.Address(key, i))
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
		"namespace":    r.ctx.Target.Namespace(),
		"installation": r.ctx.Installation,
		"component":    key,
		"service":      r.ctx.Target.Service(key),
		"replicas":     max(r.replicas(key), 1),
	}
	maps.Copy(h, instance)
	return h
}

// instanceVars are the per-replica fields of the horchestra context.
func (r *render) instanceVars(key string, i int) map[string]any {
	return map[string]any{"instance": r.ctx.Target.Instance(key, i), "index": i, "ordinal": i + 1}
}

// instanceDoc re-evaluates the per-replica formulas of a key for replica i,
// returning a copy of the key's subtree with them applied.
func (r *render) instanceDoc(key string, i int) (map[string]any, error) {
	copyDoc := schema.DeepCopy(r.doc).(map[string]any)
	instance := r.instanceVars(key, i)
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
	compiled, err := r.b.Composition.CompileWith("horchestra://composition/"+r.b.Name, r.ctx.Target.PlatformSchema())
	if err != nil {
		return err
	}
	var problems []string
	for _, v := range compiled.Validate(r.doc) {
		problems = append(problems, v.String())
	}
	for _, key := range r.keys {
		ann, _ := r.b.Composition.Root.Properties[key].KeyAnnotation()
		if len(ann.Minimum) == 0 {
			continue
		}
		keyDoc, _ := get(r.doc, "/"+key)
		km, _ := keyDoc.(map[string]any)
		platform, _ := km["platform"].(map[string]any)
		problems = append(problems, r.ctx.Target.ValidatePlatform(key, platform, ann.Minimum)...)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("values do not validate:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}
