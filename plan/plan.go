// Package plan derives the saga of a pack: the first installation, or the
// transition between two versions — which keys change and how, which hooks
// the transition selects, in what order the steps run, where the savepoints
// are and therefore how far a failure rolls back. Nobody writes the plan; the
// component authors wrote hooks, the composition author wrote savepoints, and
// this is what follows from them.
//
// The plan is a pure function of the two bundles, their renders and the
// policy. Executing it needs a cluster and is phase 2.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/sdk/packschema"
	"github.com/ks-tool/pack/sdk/semver"
)

type Kind string

const (
	Stop   Kind = "stop"
	Hook   Kind = "hook"
	Apply  Kind = "apply"
	Wait   Kind = "wait"
	Remove Kind = "remove"
)

// Step is one line of the plan.
type Step struct {
	N            int    `json:"n"`
	Key          string `json:"key"`
	Kind         Kind   `json:"kind"`
	Name         string `json:"name,omitempty"`   // hook name
	Detail       string `json:"detail,omitempty"` // what the operator should see
	Phase        string `json:"phase,omitempty"`
	Timeout      string `json:"timeout,omitempty"`
	Idempotent   bool   `json:"idempotent,omitempty"`
	Exclusive    bool   `json:"exclusive,omitempty"`
	Compensate   string `json:"compensate,omitempty"` // how it is undone; "" for an irreversible hook
	Irreversible bool   `json:"irreversible,omitempty"`
	System       bool   `json:"system,omitempty"` // a composition hook
}

// Change is what happened to one key between the two versions.
type Change struct {
	Key       string `json:"key"`
	Component string `json:"component"`
	From      string `json:"from,omitempty"` // version
	To        string `json:"to,omitempty"`
	Kind      string `json:"kind"`                // none | image | schema | config | added | removed
	Direction string `json:"direction,omitempty"` // install | upgrade | downgrade
}

// Unit is a rollback unit: the steps between two savepoints.
type Unit struct {
	From          int      `json:"from"`
	To            int      `json:"to"`
	Savepoint     string   `json:"savepoint"`
	Keys          []string `json:"keys"`
	RollbackOrder []string `json:"rollbackOrder"` // reverse plan order within the unit
	// StopsAt is where an automatic rollback of this unit stops (--no-rollback):
	// the keys before it in RollbackOrder are rolled back, it and the rest are
	// left as they are. "" — the whole unit rolls back; "*" — nothing does.
	StopsAt string `json:"stopsAt,omitempty"`
}

// Policy is what the operator decides before the saga starts; it is recorded
// with the saga and inherited by a resume.
type Policy struct {
	Savepoints    string   `json:"savepoints,omitempty"`    // "" (composition) | auto | none | k1,k2
	NoRollback    []string `json:"noRollback,omitempty"`    // where the rollback stops
	NoRollbackAll bool     `json:"noRollbackAll,omitempty"` // roll nothing back
	Strict        bool     `json:"strict,omitempty"`        // refuse a plan with an irreversible step
}

type Plan struct {
	Pack      string   `json:"pack"`
	From      string   `json:"from,omitempty"` // "" for a first installation
	To        string   `json:"to"`
	Direction string   `json:"direction"` // install | upgrade | downgrade
	Order     []string `json:"order"`
	Changes   []Change `json:"changes"`
	Steps     []Step   `json:"steps"`
	Units     []Unit   `json:"units"`
	Policy    Policy   `json:"policy"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Input is everything the plan is derived from. From nil is a first
// installation: every key is added, the install hooks run around the first
// apply. FromOut may be nil when the old version cannot be rendered with the
// new values — then config-only changes are not detected and a warning says so.
type Input struct {
	From, To       *bundle.Bundle
	FromOut, ToOut *render.Output
	Policy         Policy
}

func Build(in Input) (*Plan, error) {
	install := in.From == nil
	p := &Plan{Pack: in.To.Name, To: in.To.Version, Policy: in.Policy, Direction: "install"}
	if !install {
		p.From = in.From.Version
		p.Direction = direction(in.From.Version, in.To.Version)
	}
	order := append([]string{}, in.ToOut.Report.Order...)
	if p.Direction == "downgrade" {
		slices.Reverse(order)
	}
	p.Order = order
	if !install && in.FromOut == nil {
		p.Warnings = append(p.Warnings, "the previous version does not render with these values: config-only changes are not detected")
	}

	// changes
	changed := map[string]Change{}
	for _, key := range order {
		c, err := change(in, key)
		if err != nil {
			return nil, err
		}
		p.Changes = append(p.Changes, c)
		if c.Kind != "none" {
			changed[key] = c
		}
	}
	for _, key := range fromKeys(in) {
		if _, still := in.To.KeyComponent[key]; !still {
			c := Change{Key: key, Component: in.From.KeyComponent[key], From: in.From.Components[in.From.KeyComponent[key]].Version, Kind: "removed"}
			p.Changes = append(p.Changes, c)
			changed[key] = c
		}
	}

	// steps
	n := 0
	add := func(s Step) {
		n++
		s.N = n
		p.Steps = append(p.Steps, s)
	}
	stepsOf := map[string][]int{}
	for _, key := range order {
		c, ok := changed[key]
		if !ok {
			continue
		}
		first := len(p.Steps) + 1
		comp := in.To.Components[in.To.KeyComponent[key]]
		dir := c.Direction
		if len(dir) == 0 {
			dir = p.Direction
		}
		if c.Kind == "image" || c.Kind == "added" {
			if err := upgradeFrom(comp, c, p); err != nil {
				return nil, err
			}
		}
		pre, post := selectHooks(comp, c, dir, p)
		sysPre, sysPost := systemHooks(in.To, key, dir)
		pre = append(sysPre, pre...)
		post = append(post, sysPost...)
		if c.Kind == "config" || c.Kind == "schema" {
			pre, post = nil, nil // no version transition, no hooks
		}
		exclusive := false
		for _, h := range pre {
			exclusive = exclusive || h.Exclusive
		}
		if exclusive && dir != "install" {
			add(Step{Key: key, Kind: Stop, Detail: "exclusive hook follows"})
		}
		for _, h := range pre {
			add(hookStep(key, h, c))
		}
		add(Step{Key: key, Kind: Apply, Detail: applyDetail(c, install), Compensate: applyCompensate(c)})
		switch {
		case comp.Root.RestartPolicy == "Never":
			add(Step{Key: key, Kind: Wait, Detail: "finished (run to completion)"})
		case comp.Root.Readiness != nil:
			add(Step{Key: key, Kind: Wait, Detail: "ready (" + readiness(comp.Root.Readiness) + ")"})
		}
		for _, h := range post {
			add(hookStep(key, h, c))
		}
		for i := first; i <= len(p.Steps); i++ {
			stepsOf[key] = append(stepsOf[key], i)
		}
	}
	for _, c := range p.Changes {
		if c.Kind == "removed" {
			add(Step{Key: c.Key, Kind: Remove, Detail: "no longer in the pack; children are weeded, PVs stay", Compensate: "apply " + c.From})
			stepsOf[c.Key] = []int{len(p.Steps)}
		}
	}

	// savepoints → units
	savepoints, err := savePoints(in, changed)
	if err != nil {
		return nil, err
	}
	start := 1
	var unitKeys []string
	flush := func(sp string, end int) {
		if end < start {
			return
		}
		u := Unit{From: start, To: end, Savepoint: sp, Keys: append([]string{}, unitKeys...)}
		u.RollbackOrder = append([]string{}, unitKeys...)
		slices.Reverse(u.RollbackOrder)
		if in.Policy.NoRollbackAll {
			u.StopsAt = "*"
		} else {
			for _, k := range u.RollbackOrder {
				if slices.Contains(in.Policy.NoRollback, k) {
					u.StopsAt = k
					break
				}
			}
		}
		p.Units = append(p.Units, u)
		start = end + 1
		unitKeys = nil
	}
	for _, key := range order {
		if _, ok := changed[key]; !ok {
			continue
		}
		unitKeys = append(unitKeys, key)
		if savepoints[key] {
			flush(key, last(stepsOf[key]))
		}
	}
	for _, c := range p.Changes {
		if c.Kind == "removed" {
			unitKeys = append(unitKeys, c.Key)
		}
	}
	flush("end", len(p.Steps))

	if in.Policy.Strict {
		for _, s := range p.Steps {
			if s.Irreversible {
				return nil, fmt.Errorf("--strict: step %d (%s %s) is irreversible: no compensate", s.N, s.Key, s.Name)
			}
		}
	}
	return p, nil
}

func fromKeys(in Input) []string {
	if in.From == nil {
		return nil
	}
	return in.From.Keys
}

// change classifies one key of the new version against the old. A key the old
// version did not have — or every key of a first installation — is added: it
// is installed, whatever the direction of the rest of the plan.
func change(in Input, key string) (Change, error) {
	toComp := in.To.Components[in.To.KeyComponent[key]]
	c := Change{Key: key, Component: toComp.Name, To: toComp.Version, Kind: "none"}
	if in.From == nil {
		c.Kind, c.Direction = "added", "install"
		return c, nil
	}
	fromName, existed := in.From.KeyComponent[key]
	if !existed {
		c.Kind, c.Direction = "added", "install"
		return c, nil
	}
	fromComp := in.From.Components[fromName]
	c.From = fromComp.Version
	if fromName != toComp.Name {
		c.Kind = "image"
		c.Direction = "upgrade"
		return c, nil
	}
	c.Direction = direction(c.From, c.To)
	switch {
	case fromComp.ImageRef != toComp.ImageRef || (c.From != c.To && len(c.From) > 0 && len(c.To) > 0):
		c.Kind = "image"
	case digestOf(fromComp.SchemaRaw) != digestOf(toComp.SchemaRaw):
		c.Kind = "schema"
	case in.FromOut != nil && !sameSecrets(in.FromOut, in.ToOut, key):
		c.Kind = "config"
	}
	return c, nil
}

func sameSecrets(a, b *render.Output, key string) bool {
	names := func(o *render.Output) []string {
		var out []string
		for _, s := range o.Secrets {
			if s.Metadata.Labels[render.LabelComponent] == key {
				out = append(out, s.Metadata.Name)
			}
		}
		sort.Strings(out)
		return out
	}
	return strings.Join(names(a), ",") == strings.Join(names(b), ",")
}

func digestOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func direction(from, to string) string {
	fv, err1 := semver.Parse(from)
	tv, err2 := semver.Parse(to)
	if err1 != nil || err2 != nil {
		return "upgrade"
	}
	if tv.Compare(fv) < 0 {
		return "downgrade"
	}
	return "upgrade"
}

// upgradeFrom refuses a transition the target component does not accept.
func upgradeFrom(comp *bundle.Component, c Change, p *Plan) error {
	if comp.Root.Upgrade == nil || len(c.From) == 0 {
		return nil
	}
	cons, err := semver.ParseConstraint(comp.Root.Upgrade.From)
	if err != nil {
		return fmt.Errorf("%s: upgrade.from: %w", c.Key, err)
	}
	v, err := semver.Parse(c.From)
	if err != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s: version %q is not semver; upgrade.from not checked", c.Key, c.From))
		return nil
	}
	if !cons.Check(v) {
		return fmt.Errorf("%s: %s %s cannot be upgraded from %s (upgrade.from: %s) — go through an intermediate version", c.Key, comp.Name, c.To, c.From, comp.Root.Upgrade.From)
	}
	return nil
}

// phases are the hook phases a direction selects.
func phases(dir string) (pre, post string) {
	switch dir {
	case "install":
		return "preInstall", "postInstall"
	case "downgrade":
		return "preDowngrade", "postDowngrade"
	}
	return "preUpgrade", "postUpgrade"
}

// selectHooks picks the component's hooks for this transition: the phases of
// the direction, filtered by `when` against the two versions (an install hook
// sees only `to`).
func selectHooks(comp *bundle.Component, c Change, dir string, p *Plan) (pre, post []packschema.Hook) {
	prePhase, postPhase := phases(dir)
	for _, h := range comp.Root.Hooks {
		if h.Phase != prePhase && h.Phase != postPhase {
			continue
		}
		if h.When != nil {
			ok, known := whenMatches(h.When, c.From, c.To)
			if !known {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s: hook %s has `when` but a version is unknown (from %q, to %q); skipped", c.Key, h.Name, c.From, c.To))
				continue
			}
			if !ok {
				continue
			}
		}
		if h.Phase == prePhase {
			pre = append(pre, h)
		} else {
			post = append(post, h)
		}
	}
	return pre, post
}

func whenMatches(w *packschema.When, from, to string) (ok, known bool) {
	check := func(cons, version string) (bool, bool) {
		if len(cons) == 0 {
			return true, true
		}
		c, err := semver.ParseConstraint(cons)
		if err != nil {
			return false, true
		}
		v, err := semver.Parse(version)
		if err != nil {
			return false, false
		}
		return c.Check(v), true
	}
	okFrom, knownFrom := check(w.From, from)
	okTo, knownTo := check(w.To, to)
	if len(from) == 0 && len(w.From) == 0 { // an installation: there is no version to come from
		okFrom, knownFrom = true, true
	}
	return okFrom && okTo, knownFrom && knownTo
}

// systemHooks are the composition's hooks that name this key in `for`.
func systemHooks(b *bundle.Bundle, key, dir string) (pre, post []packschema.Hook) {
	prePhase, postPhase := phases(dir)
	for _, h := range b.Root.Hooks {
		if !slices.Contains(h.For, key) {
			continue
		}
		switch h.Phase {
		case prePhase:
			pre = append(pre, h)
		case postPhase:
			post = append(post, h)
		}
	}
	return pre, post
}

func hookStep(key string, h packschema.Hook, c Change) Step {
	s := Step{Key: key, Kind: Hook, Name: h.Name, Phase: h.Phase, Timeout: h.Timeout,
		Idempotent: h.Idempotent, Exclusive: h.Exclusive, System: len(h.For) > 0}
	switch {
	case len(c.From) > 0 && len(c.To) > 0:
		s.Detail = fmt.Sprintf("%s → %s", c.From, c.To)
	case len(c.To) > 0:
		s.Detail = c.To
	}
	if h.Compensate != nil {
		s.Compensate = strings.Join(h.Compensate.Command, " ")
	} else if !h.Idempotent {
		s.Irreversible = true
	}
	return s
}

func applyDetail(c Change, install bool) string {
	switch c.Kind {
	case "added":
		if install {
			return c.To
		}
		return strings.TrimSpace(c.To + " (new key)")
	case "config":
		return "config only"
	case "schema":
		return "schema changed, image the same"
	}
	if len(c.To) > 0 {
		return c.To
	}
	return "image changed"
}

// applyCompensate is how an apply is undone: the previous revision, or — for
// a key that did not exist — removal (the children are weeded, the PVs stay).
func applyCompensate(c Change) string {
	switch {
	case c.Kind == "added":
		return "remove (PVs stay)"
	case len(c.From) > 0:
		return "apply " + c.From
	}
	return "apply the previous revision"
}

func readiness(r *packschema.Readiness) string {
	switch {
	case len(r.TCP) > 0:
		return "tcp:" + r.TCP
	case len(r.HTTP) > 0:
		return "http:" + r.HTTP
	}
	return "exec"
}

// savePoints resolves the policy: the composition's list, or auto (every
// changed key), or none, or the operator's list.
func savePoints(in Input, changed map[string]Change) (map[string]bool, error) {
	out := map[string]bool{}
	spec := in.Policy.Savepoints
	var list []string
	switch spec {
	case "auto":
		for k := range changed {
			out[k] = true
		}
		return out, nil
	case "none":
		return out, nil
	case "":
		list = in.To.Root.Savepoints
		if len(list) == 0 {
			for k := range changed {
				out[k] = true
			}
			return out, nil
		}
	default:
		list = strings.Split(spec, ",")
	}
	for _, k := range list {
		k = strings.TrimSpace(k)
		if _, ok := in.To.KeyComponent[k]; !ok {
			return nil, fmt.Errorf("savepoint %q is not a key of the composition", k)
		}
		out[k] = true
	}
	return out, nil
}

// Text renders the plan the way the operator reads it before saying yes.
func (p *Plan) Text() string {
	var b strings.Builder
	if p.Direction == "install" {
		_, _ = fmt.Fprintf(&b, "%s: install %s\n", p.Pack, orUnknown(p.To))
	} else {
		_, _ = fmt.Fprintf(&b, "%s: %s → %s (%s)\n", p.Pack, orUnknown(p.From), orUnknown(p.To), p.Direction)
	}
	unchanged := 0
	for _, c := range p.Changes {
		if c.Kind == "none" {
			unchanged++
		}
	}
	if unchanged > 0 {
		_, _ = fmt.Fprintf(&b, "unchanged: %d key(s)\n", unchanged)
	}
	if len(p.Steps) == 0 {
		b.WriteString("nothing to do\n")
		return b.String()
	}
	unitEnd := map[int]Unit{}
	for _, u := range p.Units {
		unitEnd[u.To] = u
	}
	var irreversible []string
	for _, s := range p.Steps {
		right := s.Detail
		switch s.Kind {
		case Hook:
			extra := ""
			if len(s.Compensate) > 0 {
				extra = "compensate: " + s.Compensate
			} else if s.Idempotent {
				extra = "idempotent"
			} else {
				extra = "IRREVERSIBLE"
				irreversible = append(irreversible, fmt.Sprint(s.N))
			}
			if len(s.Timeout) > 0 {
				extra += "   ~" + s.Timeout
			}
			name := s.Name
			if s.System {
				name += " (composition)"
			}
			if len(s.Detail) > 0 {
				name += " (" + s.Detail + ")"
			}
			right = fmt.Sprintf("%-40s %s", name, extra)
			_, _ = fmt.Fprintf(&b, "%3d. %-16s hook  %s\n", s.N, s.Key, right)
		default:
			_, _ = fmt.Fprintf(&b, "%3d. %-16s %-5s %s\n", s.N, s.Key, s.Kind, right)
		}
		if u, ok := unitEnd[s.N]; ok {
			line := fmt.Sprintf("     ── savepoint: %s ──   unit %d–%d", u.Savepoint, u.From, u.To)
			if u.Savepoint == "end" {
				line = fmt.Sprintf("     ── end ──   unit %d–%d", u.From, u.To)
			}
			if len(u.RollbackOrder) > 1 {
				line += "; rollback order: " + strings.Join(u.RollbackOrder, " → ")
			}
			switch u.StopsAt {
			case "":
			case "*":
				line += "; NO automatic rollback"
			default:
				line += "; rollback stops at " + u.StopsAt
			}
			b.WriteString(line + "\n")
		}
	}
	policy := "auto, to last savepoint"
	switch {
	case p.Policy.NoRollbackAll:
		policy = "none"
	case len(p.Policy.NoRollback) > 0:
		policy = "auto, stops at " + strings.Join(p.Policy.NoRollback, ", ")
	}
	irr := "none"
	if len(irreversible) > 0 {
		irr = "steps " + strings.Join(irreversible, ", ")
	}
	_, _ = fmt.Fprintf(&b, "rollback policy: %s; irreversible: %s\n", policy, irr)
	for _, w := range p.Warnings {
		_, _ = fmt.Fprintf(&b, "warning: %s\n", w)
	}
	return b.String()
}

func orUnknown(v string) string {
	if len(v) == 0 {
		return "?"
	}
	return v
}

func last(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	return xs[len(xs)-1]
}
