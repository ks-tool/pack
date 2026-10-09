// Package saga drives an installation or an upgrade to its end: pack performs
// and controls every step of the plan; the target only carries each one out
// in its own system (target.Executor) — a StatefulSet applied to a cluster,
// config files copied to a host and an image started there.
//
// The saga writes the installation's record through the target after every
// step, so what is installed, and how far a saga got, lives with the target —
// pack keeps no state of its own. A step that fails rolls its rollback unit
// back (plan.Unit): the keys of the unit in reverse plan order, up to where
// the policy stops (StopsAt); units before it — behind a savepoint — stay.
package saga

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ks-tool/pack/plan"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

// Record is pack's record of an installation, kept by the target verbatim.
type Record struct {
	// Pack is where the installed version came from: a directory or cr://.
	Pack    string        `json:"pack"`
	Name    string        `json:"name"`
	Version string        `json:"version,omitempty"`
	Values  render.Values `json:"values"`
	// State: installed — the version above is in place; in-progress — a
	// saga towards Saga.Pack is running or was cut short; failed — it failed,
	// and Saga says what was rolled back.
	State string    `json:"state"`
	Saga  *Progress `json:"saga,omitempty"`
}

const (
	Installed  = "installed"
	InProgress = "in-progress"
	Failed     = "failed"
)

// Progress is a saga under way, or the last one that failed.
type Progress struct {
	Pack    string        `json:"pack"`
	Version string        `json:"version,omitempty"`
	Values  render.Values `json:"values"`
	// Given is the values document as the operator gave it — before the
	// tool wrote references to the credentials it provisioned: what the same
	// command gives again when it continues the saga.
	Given  render.Values `json:"given"`
	Policy plan.Policy   `json:"policy"`
	// Done are the steps that are in effect: performed and not undone.
	Done       []int     `json:"done,omitempty"`
	Failed     int       `json:"failed,omitempty"`
	Error      string    `json:"error,omitempty"`
	RolledBack []string  `json:"rolledBack,omitempty"` // steps undone, as "<kind> <key>", in order
	Started    time.Time `json:"started"`
}

// Unfinished reports whether the record holds a saga that did not end.
func (r *Record) Unfinished() bool { return r != nil && r.State != Installed && r.Saga != nil }

// Read returns the target's record of the installation; nil when nothing is installed.
func Read(x target.Executor) (*Record, error) {
	raw, exists, err := x.ReadInstalled()
	if err != nil || !exists {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("the installation's record: %w", err)
	}
	return &r, nil
}

// Write keeps a record with the target.
func Write(x target.Executor, r *Record) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return x.WriteInstalled(raw)
}

// Run is one saga: the plan from the installed release (From, nil for a
// first installation) to the target one (To).
type Run struct {
	Plan        *plan.Plan
	From, To    *render.Release
	Credentials []render.Credential
	Exec        target.Executor
	// Record is written after every step. The caller sets Record.Saga — where
	// the saga goes — and Execute keeps State and the progress.
	Record *Record
	// Events hears what happens, a line at a time.
	Events func(string)
}

// Execute performs the plan step by step. A saga the record shows unfinished
// is continued from its last savepoint: the steps in effect of the unit it
// stopped in are undone, and it runs again from that unit's first step —
// a unit is all or nothing. On success the record says the new version is
// installed; on a failure the step's unit is rolled back and the record
// says what failed and what was undone.
func (r *Run) Execute(ctx context.Context) error {
	if r.Events == nil {
		r.Events = func(string) {}
	}
	if r.Record == nil || r.Record.Saga == nil {
		return fmt.Errorf("a saga needs its record and where it goes (Record.Saga)")
	}
	start := 1
	if len(r.Record.Saga.Done) > 0 {
		var err error
		if start, err = r.resume(ctx); err != nil {
			return err
		}
	}
	r.Record.State = InProgress
	if err := Write(r.Exec, r.Record); err != nil {
		return fmt.Errorf("record the saga: %w", err)
	}
	for _, s := range r.Plan.Steps {
		if s.N < start {
			continue
		}
		r.Events(strings.TrimSpace(fmt.Sprintf("%d. %s %s %s", s.N, s.Key, s.Kind, s.Name)))
		err := r.step(ctx, r.forward(s))
		if err == nil {
			r.setDone(s.N, true)
			if err := Write(r.Exec, r.Record); err != nil {
				return fmt.Errorf("record step %d: %w", s.N, err)
			}
			continue
		}
		r.Record.State = Failed
		r.Record.Saga.Failed, r.Record.Saga.Error = s.N, err.Error()
		r.Events(fmt.Sprintf("step %d failed: %v", s.N, err))
		var rbErr error
		if u, ok := unitOf(r.Plan, s.N); ok {
			performed := func(x plan.Step) bool {
				return r.done(x.N) || (x.N == s.N && (x.Kind == plan.Apply || x.Kind == plan.Remove))
			}
			rbErr = r.undoUnit(ctx, u, performed, u.StopsAt)
		}
		if werr := Write(r.Exec, r.Record); werr != nil && rbErr == nil {
			rbErr = fmt.Errorf("record the failure: %w", werr)
		}
		if rbErr != nil {
			return fmt.Errorf("step %d (%s %s) failed: %v; rollback: %w", s.N, s.Kind, s.Key, err, rbErr)
		}
		return fmt.Errorf("step %d (%s %s) failed: %v; rolled back: %s", s.N, s.Kind, s.Key, err, rolledBack(r.Record.Saga.RolledBack))
	}
	sg := r.Record.Saga
	r.Record.Pack, r.Record.Version, r.Record.Values = sg.Pack, sg.Version, sg.Values
	r.Record.State, r.Record.Saga = Installed, nil
	return Write(r.Exec, r.Record)
}

// resume finds the unit the saga stopped in — the first step not in effect
// — undoes what of it is in effect, whatever the rollback policy left, and
// returns its first step. Every step before it must be in effect: the units
// behind the savepoint.
func (r *Run) resume(ctx context.Context) (int, error) {
	first := 0
	for _, s := range r.Plan.Steps {
		if !r.done(s.N) {
			first = s.N
			break
		}
	}
	if first == 0 {
		return len(r.Plan.Steps) + 1, nil // everything in effect: only the record is left to write
	}
	u, ok := unitOf(r.Plan, first)
	if !ok {
		return 0, fmt.Errorf("step %d is in no unit of the plan", first)
	}
	for _, s := range r.Plan.Steps {
		if s.N < u.From && !r.done(s.N) {
			return 0, fmt.Errorf("the saga's record does not match the plan: step %d, before the savepoint, is not in effect", s.N)
		}
	}
	r.Events(fmt.Sprintf("continuing from the savepoint before step %d", u.From))
	if err := r.undoUnit(ctx, u, func(s plan.Step) bool { return r.done(s.N) }, ""); err != nil {
		_ = Write(r.Exec, r.Record)
		return 0, fmt.Errorf("undo the unfinished unit: %w", err)
	}
	r.Record.Saga.Failed, r.Record.Saga.Error = 0, ""
	return u.From, nil
}

func (r *Run) done(n int) bool { return slices.Contains(r.Record.Saga.Done, n) }

func (r *Run) setDone(n int, in bool) {
	d := slices.DeleteFunc(r.Record.Saga.Done, func(x int) bool { return x == n })
	if in {
		d = append(d, n)
		slices.Sort(d)
	}
	r.Record.Saga.Done = d
}

func rolledBack(steps []string) string {
	if len(steps) == 0 {
		return "nothing"
	}
	return strings.Join(steps, ", ")
}

// step runs one step under its timeout.
func (r *Run) step(ctx context.Context, st target.Step) error {
	if len(st.Timeout) > 0 {
		d, err := time.ParseDuration(st.Timeout)
		if err != nil {
			return fmt.Errorf("timeout %q: %w", st.Timeout, err)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	return r.Exec.Execute(ctx, st, func(msg string) { r.Events("   " + msg) })
}

// forward is a plan step as the target performs it.
func (r *Run) forward(s plan.Step) target.Step {
	st := target.Step{N: s.N, Key: s.Key, Kind: string(s.Kind), Name: s.Name, Phase: s.Phase, Timeout: s.Timeout,
		Exclusive: s.Exclusive, Image: s.Image, Command: s.Command,
		Unit: unit(r.To, s.Key), Previous: unit(r.From, s.Key), Credentials: r.credentials(s.Key)}
	if s.Kind == plan.Remove {
		st.Unit = nil // nothing of the key is in the target version
	}
	return st
}

func (r *Run) credentials(key string) []render.Credential {
	var out []render.Credential
	for _, c := range r.Credentials {
		if c.Key == key {
			out = append(out, c)
		}
	}
	return out
}

func unit(rel *render.Release, key string) *render.Unit {
	if rel == nil {
		return nil
	}
	for i := range rel.Units {
		if rel.Units[i].Key == key {
			return &rel.Units[i]
		}
	}
	return nil
}

// undoUnit undoes what of a unit is performed: its keys in RollbackOrder, up
// to stopsAt ("*" — nothing). For a key, in reverse order of its steps: a
// hook's compensation; an apply undone by the previous unit, or — the key
// was added — by its removal; a remove undone by applying the previous unit;
// a key stopped for an exclusive hook and never applied started again from
// the previous unit. Each undone step leaves Done, and the record is written.
func (r *Run) undoUnit(ctx context.Context, u plan.Unit, performed func(plan.Step) bool, stopsAt string) error {
	if stopsAt == "*" {
		return nil
	}
	for _, key := range u.RollbackOrder {
		if key == stopsAt {
			break
		}
		var steps []plan.Step
		for _, s := range r.Plan.Steps {
			if s.Key == key && s.N >= u.From && s.N <= u.To && performed(s) {
				steps = append(steps, s)
			}
		}
		slices.Reverse(steps)
		applied := slices.ContainsFunc(steps, func(s plan.Step) bool { return s.Kind == plan.Apply })
		for _, s := range steps {
			for _, st := range r.undo(s, applied) {
				r.Events(strings.TrimSpace(fmt.Sprintf("rollback: %s %s %s", key, st.Kind, st.Name)))
				if err := r.step(ctx, st); err != nil {
					return fmt.Errorf("undo step %d (%s %s): %w", s.N, s.Kind, key, err)
				}
				r.Record.Saga.RolledBack = append(r.Record.Saga.RolledBack, strings.TrimSpace(st.Kind+" "+key+" "+st.Name))
			}
			r.setDone(s.N, false)
			if err := Write(r.Exec, r.Record); err != nil {
				return fmt.Errorf("record the undo of step %d: %w", s.N, err)
			}
		}
	}
	return nil
}

// undo is what undoes s: nothing, a compensation, or — for an apply of a
// key that changed version — the plan's downgrade hooks around applying the
// previous unit.
func (r *Run) undo(s plan.Step, applied bool) []target.Step {
	to, prev := unit(r.To, s.Key), unit(r.From, s.Key)
	base := target.Step{N: s.N, Key: s.Key, Rollback: true, Timeout: s.Timeout, Credentials: r.credentials(s.Key)}
	switch s.Kind {
	case plan.Hook:
		if len(s.CompensateCommand) == 0 {
			return nil
		}
		base.Kind, base.Name, base.Phase, base.Image, base.Command = string(plan.Hook), s.Name, s.Phase, s.CompensateImage, s.CompensateCommand
		base.Unit, base.Previous = to, prev
		return []target.Step{base}
	case plan.Apply:
		if prev == nil {
			base.Kind, base.Previous = string(plan.Remove), to // the key was added: it goes
			return []target.Step{base}
		}
		apply := base
		apply.Kind, apply.Unit, apply.Previous = string(plan.Apply), prev, to
		kr, ok := r.Plan.Rollback[s.Key]
		if !ok {
			return []target.Step{apply}
		}
		hook := func(h plan.Step) target.Step {
			st := base
			st.Kind, st.Name, st.Phase, st.Timeout, st.Exclusive = string(plan.Hook), h.Name, h.Phase, h.Timeout, h.Exclusive
			st.Image, st.Command, st.Unit, st.Previous = h.Image, h.Command, prev, to
			return st
		}
		var out []target.Step
		if kr.Stop {
			stop := base
			stop.Kind, stop.Unit, stop.Previous = string(plan.Stop), to, prev
			out = append(out, stop)
		}
		for _, h := range kr.Pre {
			out = append(out, hook(h))
		}
		out = append(out, apply)
		for _, h := range kr.Post {
			out = append(out, hook(h))
		}
		return out
	case plan.Remove:
		if prev == nil {
			return nil
		}
		base.Kind, base.Unit = string(plan.Apply), prev
		return []target.Step{base}
	case plan.Stop:
		if applied || prev == nil {
			return nil // the apply's own undo starts it again
		}
		base.Kind, base.Unit, base.Previous = string(plan.Apply), prev, to
		return []target.Step{base}
	}
	return nil
}

func unitOf(p *plan.Plan, n int) (plan.Unit, bool) {
	for _, u := range p.Units {
		if n >= u.From && n <= u.To {
			return u, true
		}
	}
	return plan.Unit{}, false
}
