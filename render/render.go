package render

import (
	"errors"
	"fmt"
	"maps"
	"slices"
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
	// Installed are the target's halves of the keys' platforms as the target
	// resolved them for the installed version (Release.Platforms, kept in the
	// record): rendering that version again, a key in it is resolved as it
	// was, whatever the environment says now.
	Installed map[string]map[string]any
}

// Target is what the render asks of the system it renders for. A key's
// platform is two halves of one schema: the pack's — resources (cgroups, the
// same on every target), env, metadata — which the render reads itself, and
// the target's — replicas, placement, whatever its scheduler has words for —
// which the target fills and the render passes on. A pack is rendered for
// one target, never for "any": a neighbour's address ends up in a config file.
type Target interface {
	// PlatformSchema is the target's half of the platform schema; nil — it
	// adds nothing. platform.Merge joins it to the pack's.
	PlatformSchema() []byte
	// Namespace is the scope the target installs into; empty where it has none.
	Namespace() string
	// Resolve says what a selected key's platform means on the target. scale
	// is the component's x-pack.scale; a key that is not replicas has no
	// instances of its own and is addressed as one service. installed: the
	// platform's target half is what Resolve returned when the installed
	// version was installed — it is resolved as it says, the environment and
	// the target's defaults not consulted.
	Resolve(key, scale string, platform map[string]any, installed bool) (*Resolved, error)
	// ValidatePlatform checks a key's platform in the target's own terms,
	// one "pointer: message" per problem.
	ValidatePlatform(key string, platform map[string]any) []string
}

// Resolved is a key as the target will run it.
type Resolved struct {
	// Service is the key's stable name; Address how a neighbour reaches it
	// as one service.
	Service string
	Address string
	// Instances are its replicas in order — scale: replicas only.
	Instances []Instance
	// Platform is the target's half of the key's platform as the target will
	// run it — what the operator wrote, what the environment says, the
	// target's defaults: it replaces the target's properties in the document.
	Platform map[string]any
}

// Instance is one replica: its name and how a neighbour reaches it.
type Instance struct {
	Name    string
	Address string
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
	instance bool // reads pack.instance/index/ordinal — evaluated per replica only
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
	// resolved is the target's answer about each selected key, asked once
	// the platform subtrees are settled (resolve); limits are what formulas
	// read of each key's resources.
	resolved map[string]*Resolved
	limits   map[string]map[string]any
	// keysVar is pack.keys: every selected key as `me` describes it, built
	// once the target has resolved them all
	keysVar map[string]any
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
			if rd == "pack.instance" || rd == "pack.index" || rd == "pack.ordinal" {
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
	// pack.values is every key's values: it waits for every formula that does not read it
	// too — those cannot wait for each other, and see each other's leaves unsettled
	if readsPackValues(f.reads) {
		for p, g := range pending {
			if p != f.ptr && !readsPackValues(g.reads) {
				return true
			}
		}
	}
	for _, rd := range f.reads {
		var target string
		if rel, ok := strings.CutPrefix(rd, "values."); ok {
			target = f.root + "/" + strings.ReplaceAll(rel, ".", "/")
		} else if role, rest, ok := peerValuesRead(rd); ok {
			// a neighbour's value waits for the neighbour's own formula
			peer, bound := r.peers[f.key][role]
			if !bound {
				continue
			}
			target = "/" + peer
			if len(rest) > 0 {
				target += "/" + strings.ReplaceAll(rest, ".", "/")
			}
		} else {
			continue
		}
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
// per-replica fields to the pack context.
func (r *render) eval(f *formula, doc map[string]any, instance map[string]any) (any, error) {
	sub, _ := get(doc, f.root)
	keyDoc, _ := get(doc, "/"+f.key)
	keyMap, _ := keyDoc.(map[string]any)
	if keyMap == nil {
		keyMap = map[string]any{}
	}
	common, _ := doc["common"].(map[string]any)
	vars := map[string]any{
		"values": orEmpty(sub),
		"limits": orEmpty(r.limits[f.key]),
		"common": orEmpty(common),
		"me":     r.me(f.key),
		"peers":  r.peerVars(f.key, doc, readsPeerValues(f.reads)),
		"pack":   r.packContext(f.key, instance),
	}
	if readsPackValues(f.reads) {
		vars["pack"].(map[string]any)["values"] = r.allValues(doc)
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

// resolve asks the target what each selected key's platform means: its
// names and addresses, its instances (scale: replicas only — single is one,
// addressed as the key) and the limits formulas may read.
func (r *render) resolve() error {
	r.resolved, r.limits = map[string]*Resolved{}, map[string]map[string]any{}
	for _, key := range r.keys {
		keyDoc, _ := get(r.doc, "/"+key)
		km, _ := keyDoc.(map[string]any)
		pl, _ := km["platform"].(map[string]any)
		was, installed := r.ctx.Installed[key]
		if installed {
			// the document's pack half, the target's half as it was
			p := map[string]any{}
			maps.Copy(p, pl)
			maps.Copy(p, was)
			pl = p
		}
		res, err := r.ctx.Target.Resolve(key, r.b.Components[r.b.KeyComponent[key]].Root.Scale, pl, installed)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		r.resolved[key] = res
		// the document as it is, the target's half as the target says — what
		// neither half knows stays, for the schema to refuse
		merged := map[string]any{}
		maps.Copy(merged, pl)
		maps.Copy(merged, res.Platform)
		if km != nil && (len(merged) > 0 || pl != nil) {
			km["platform"] = merged
		}
		if r.limits[key], err = limitsOf(merged); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

// limitsOf is what formulas read as `limits`: platform.resources.limits,
// memory in bytes, cpu in millicores — cgroups, the pack's own words.
func limitsOf(pl map[string]any) (map[string]any, error) {
	res, _ := pl["resources"].(map[string]any)
	lim, _ := res["limits"].(map[string]any)
	out := map[string]any{}
	for name, div := range map[string]int64{"memory": 1000, "cpu": 1} {
		v, ok := lim[name].(string)
		if !ok {
			continue
		}
		q, err := compute.Quantity(v)
		if err != nil {
			return nil, fmt.Errorf("platform.resources.limits.%s: %w", name, err)
		}
		out[name] = q / div
	}
	return out, nil
}

// replicas is 0 for a key that does not scale, n ≥ 1 for scale: replicas.
func (r *render) replicas(key string) int { return len(r.resolved[key].Instances) }

// me describes a key by names only: service, instances, replicas, ports.
func (r *render) me(key string) map[string]any {
	res := r.resolved[key]
	svc, n := res.Service, len(res.Instances)
	var instances []any
	if n == 0 {
		instances = []any{res.Address}
	} else {
		for _, in := range res.Instances {
			instances = append(instances, in.Address)
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

// peerVars are the neighbours a key's formulas see, by role: names and ports
// always; their values (peers.<role>.values) only where a formula reads them,
// off doc as it stands.
func (r *render) peerVars(key string, doc map[string]any, withValues bool) map[string]any {
	out := map[string]any{}
	for role, peer := range r.peers[key] {
		m := r.me(peer)
		if withValues {
			m["values"] = r.neighbourValues(peer, doc)
		}
		out[role] = m
	}
	return out
}

// neighbourValues is a neighbour key's subtree of doc — what the operator
// wrote, the defaults, and the formulas settled so far — less every password
// leaf: a derived value is stored, a credential must not be one. Leaves that
// differ per replica are not in it; they exist only inside the replica's file.
func (r *render) neighbourValues(peer string, doc map[string]any) map[string]any {
	sub, _ := get(doc, "/"+peer)
	m, _ := schema.DeepCopy(sub).(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	r.dropPasswords("/"+peer, m)
	return m
}

func (r *render) dropPasswords(ptr string, m map[string]any) {
	for k, v := range m {
		p := schema.Pointer(append(schema.Split(ptr), k)...)
		if _, n, err := r.b.Composition.Lookup(p); err == nil && n != nil && n.IsPassword() {
			delete(m, k)
			continue
		}
		if sub, ok := v.(map[string]any); ok {
			r.dropPasswords(p, sub)
		}
	}
}

// allValues is pack.values: the values of every key of the installation, as
// neighbourValues has them — no passwords, no per-replica leaves.
func (r *render) allValues(doc map[string]any) map[string]any {
	out := make(map[string]any, len(r.keys))
	for _, k := range r.keys {
		out[k] = r.neighbourValues(k, doc)
	}
	return out
}

func readsPackValues(reads []string) bool {
	return slices.ContainsFunc(reads, func(rd string) bool { return rd == "pack.values" || strings.HasPrefix(rd, "pack.values.") })
}

// peerValuesRead splits a read of a neighbour's values — peers.<role>.values
// or peers.<role>.values.<path> — into the role and the path under values.
func peerValuesRead(rd string) (role, rest string, ok bool) {
	tail, found := strings.CutPrefix(rd, "peers.")
	if !found {
		return "", "", false
	}
	role, tail, _ = strings.Cut(tail, ".")
	switch {
	case tail == "values":
		return role, "", true
	case strings.HasPrefix(tail, "values."):
		rest = strings.TrimPrefix(tail, "values.")
		if i := strings.Index(rest, "[]"); i >= 0 { // a dynamic index: the path up to it
			rest = strings.TrimSuffix(rest[:i], ".")
		}
		return role, rest, true
	}
	return "", "", false
}

func readsPeerValues(reads []string) bool {
	return slices.ContainsFunc(reads, func(rd string) bool { _, _, ok := peerValuesRead(rd); return ok })
}

// packContext is what formulas and templates read as `pack`: where and as
// what this key is installed — and, per replica, which one.
func (r *render) packContext(key string, instance map[string]any) map[string]any {
	h := map[string]any{
		"namespace":    r.ctx.Target.Namespace(),
		"installation": r.ctx.Installation,
		"component":    key,
		"service":      r.resolved[key].Service,
		"replicas":     max(r.replicas(key), 1),
		"keys":         r.keysContext(),
	}
	maps.Copy(h, instance)
	return h
}

// keysContext is pack.keys: every key of the installation by name, as `me`
// describes it, plus `scaled` — whether its instances are replicas (a
// per-instance Secret key carries the index). Reading it binds nothing: a
// key that names every host of the installation (a CA, a realm's plan) runs
// before them, which a peer could not.
func (r *render) keysContext() map[string]any {
	if r.keysVar == nil {
		r.keysVar = map[string]any{}
		for _, k := range r.keys {
			m := r.me(k)
			m["scaled"] = r.replicas(k) > 0
			r.keysVar[k] = m
		}
	}
	return r.keysVar
}

// instanceVars are the per-replica fields of the pack context.
func (r *render) instanceVars(key string, i int) map[string]any {
	name := r.resolved[key].Service // a key that does not scale is its one instance
	if in := r.resolved[key].Instances; i < len(in) {
		name = in[i].Name
	}
	return map[string]any{"instance": name, "index": i, "ordinal": i + 1}
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
		keyDoc, _ := get(r.doc, "/"+key)
		km, _ := keyDoc.(map[string]any)
		pl, _ := km["platform"].(map[string]any)
		problems = append(problems, r.ctx.Target.ValidatePlatform(key, pl)...)
		ann, _ := r.b.Composition.Root.Properties[key].KeyAnnotation()
		res, _ := pl["resources"].(map[string]any)
		req, _ := res["requests"].(map[string]any)
		for name, min := range ann.Minimum {
			have, ok := req[name].(string)
			if !ok {
				problems = append(problems, fmt.Sprintf("/%s/platform/resources/requests/%s: the composition requires at least %s", key, name, min))
				continue
			}
			hv, err1 := compute.Quantity(have)
			mv, err2 := compute.Quantity(min)
			if err1 == nil && err2 == nil && hv < mv {
				problems = append(problems, fmt.Sprintf("/%s/platform/resources/requests/%s: %s is below the composition's minimum %s", key, name, have, min))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("values do not validate:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}
