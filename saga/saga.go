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
	Pack       string        `json:"pack"`
	Version    string        `json:"version,omitempty"`
	Values     render.Values `json:"values"`
	Policy     plan.Policy   `json:"policy"`
	Done       []int         `json:"done,omitempty"`
	Failed     int           `json:"failed,omitempty"`
	Error      string        `json:"error,omitempty"`
	RolledBack []string      `json:"rolledBack,omitempty"` // steps undone, as "<kind> <key>"
	Started    time.Time     `json:"started"`
}

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

// Execute performs the plan step by step. On success the record says the new
// version is installed; on a failure the step's rollback unit is rolled back
// and the record says what failed and what was undone.
func (r *Run) Execute(ctx context.Context) error {
	if r.Events == nil {
		r.Events = func(string) {}
	}
	if r.Record == nil || r.Record.Saga == nil {
		return fmt.Errorf("a saga needs its record and where it goes (Record.Saga)")
	}
	r.Record.State = InProgress
	if err := Write(r.Exec, r.Record); err != nil {
		return fmt.Errorf("record the saga: %w", err)
	}
	done := map[int]bool{}
	for _, s := range r.Plan.Steps {
		r.Events(strings.TrimSpace(fmt.Sprintf("%d. %s %s %s", s.N, s.Key, s.Kind, s.Name)))
		err := r.step(ctx, r.forward(s))
		if err == nil {
			done[s.N] = true
			r.Record.Saga.Done = append(r.Record.Saga.Done, s.N)
			if err := Write(r.Exec, r.Record); err != nil {
				return fmt.Errorf("record step %d: %w", s.N, err)
			}
			continue
		}
		r.Record.State = Failed
		r.Record.Saga.Failed, r.Record.Saga.Error = s.N, err.Error()
		r.Events(fmt.Sprintf("step %d failed: %v", s.N, err))
		rbErr := r.rollback(ctx, s, done)
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

// rollback undoes the failed step's unit: its keys in RollbackOrder, up to
// StopsAt. For a key, in reverse order of its steps: a hook's compensation;
// an apply undone by the previous unit, or — the key was added — by its
// removal; a remove undone by applying the previous unit; a key stopped for
// an exclusive hook and never applied started again from the previous unit.
// The failed step counts as performed for an apply or a remove — it may have
// half-happened — and never for a hook.
func (r *Run) rollback(ctx context.Context, failed plan.Step, done map[int]bool) error {
	u, ok := unitOf(r.Plan, failed.N)
	if !ok || u.StopsAt == "*" {
		return nil
	}
	attempted := func(s plan.Step) bool {
		return done[s.N] || (s.N == failed.N && (s.Kind == plan.Apply || s.Kind == plan.Remove))
	}
	for _, key := range u.RollbackOrder {
		if key == u.StopsAt {
			break
		}
		var steps []plan.Step
		for _, s := range r.Plan.Steps {
			if s.Key == key && s.N >= u.From && s.N <= u.To && attempted(s) {
				steps = append(steps, s)
			}
		}
		slices.Reverse(steps)
		applied := slices.ContainsFunc(steps, func(s plan.Step) bool { return s.Kind == plan.Apply })
		for _, s := range steps {
			st, undo := r.undo(s, applied)
			if !undo {
				continue
			}
			r.Events(strings.TrimSpace(fmt.Sprintf("rollback: %s %s %s", key, st.Kind, st.Name)))
			if err := r.step(ctx, st); err != nil {
				return fmt.Errorf("undo step %d (%s %s): %w", s.N, s.Kind, key, err)
			}
			r.Record.Saga.RolledBack = append(r.Record.Saga.RolledBack, st.Kind+" "+key)
		}
	}
	return nil
}

// undo is the step that undoes s, if any.
func (r *Run) undo(s plan.Step, applied bool) (target.Step, bool) {
	to, prev := unit(r.To, s.Key), unit(r.From, s.Key)
	base := target.Step{N: s.N, Key: s.Key, Rollback: true, Timeout: s.Timeout, Credentials: r.credentials(s.Key)}
	switch s.Kind {
	case plan.Hook:
		if len(s.CompensateCommand) == 0 {
			return base, false
		}
		base.Kind, base.Name, base.Phase, base.Image, base.Command = string(plan.Hook), s.Name, s.Phase, s.CompensateImage, s.CompensateCommand
		base.Unit, base.Previous = to, prev
		return base, true
	case plan.Apply:
		if prev == nil {
			base.Kind, base.Previous = string(plan.Remove), to // the key was added: it goes
			return base, true
		}
		base.Kind, base.Unit, base.Previous = string(plan.Apply), prev, to
		return base, true
	case plan.Remove:
		base.Kind, base.Unit = string(plan.Apply), prev
		return base, prev != nil
	case plan.Stop:
		if applied || prev == nil {
			return base, false // the apply's own undo starts it again
		}
		base.Kind, base.Unit, base.Previous = string(plan.Apply), prev, to
		return base, true
	}
	return base, false
}

func unitOf(p *plan.Plan, n int) (plan.Unit, bool) {
	for _, u := range p.Units {
		if n >= u.From && n <= u.To {
			return u, true
		}
	}
	return plan.Unit{}, false
}
