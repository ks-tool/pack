package saga

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/ks-tool/pack/plan"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

// memory is a target that performs nothing: it logs what it was asked, fails
// the step named in fail, and keeps the record in memory.
type memory struct {
	log    []string
	fail   string // "<kind> <key>"
	record []byte
	writes int
}

func (m *memory) ReadInstalled() ([]byte, bool, error) { return m.record, m.record != nil, nil }
func (m *memory) WriteInstalled(r []byte) error        { m.record = r; m.writes++; return nil }
func (m *memory) Execute(_ context.Context, s target.Step, events func(string)) error {
	what := s.Kind + " " + s.Key
	if s.Rollback {
		what = "undo " + what
	}
	if s.Kind == "apply" && s.Unit != nil {
		what += " " + s.Unit.Image
	}
	if s.Kind == "hook" {
		what += " " + s.Image
	}
	m.log = append(m.log, what)
	events("did " + what)
	if what == m.fail {
		return errors.New("boom")
	}
	return nil
}

var _ target.Executor = (*memory)(nil)

func rel(version string, keys ...string) *render.Release {
	r := &render.Release{Version: version}
	for _, k := range keys {
		r.Units = append(r.Units, render.Unit{Key: k, Image: k + ":" + version})
	}
	return r
}

// a (unit 1, savepoint a) is upgraded; b is added with a compensable hook,
// c upgraded — unit 2.
func upgrade() (*plan.Plan, *render.Release, *render.Release) {
	p := &plan.Plan{To: "2", Steps: []plan.Step{
		{N: 1, Key: "a", Kind: plan.Apply},
		{N: 2, Key: "a", Kind: plan.Wait},
		{N: 3, Key: "b", Kind: plan.Apply},
		{N: 4, Key: "b", Kind: plan.Hook, Name: "init", Image: "b:2", Command: []string{"init"},
			CompensateImage: "b:2", CompensateCommand: []string{"uninit"}},
		{N: 5, Key: "c", Kind: plan.Hook, Name: "migrate", Image: "c:2", Command: []string{"migrate"}},
		{N: 6, Key: "c", Kind: plan.Apply},
		{N: 7, Key: "c", Kind: plan.Wait},
	}, Units: []plan.Unit{
		{From: 1, To: 2, Savepoint: "a", Keys: []string{"a"}, RollbackOrder: []string{"a"}},
		{From: 3, To: 7, Savepoint: "end", Keys: []string{"b", "c"}, RollbackOrder: []string{"c", "b"}},
	}}
	return p, rel("1", "a", "c"), rel("2", "a", "b", "c")
}

func run(t *testing.T, m *memory, p *plan.Plan, from, to *render.Release) (*Record, error) {
	t.Helper()
	rec := &Record{Pack: "old", Name: "x", Version: "1", State: Installed,
		Saga: &Progress{Pack: "new", Version: "2"}}
	var events []string
	r := &Run{Plan: p, From: from, To: to, Exec: m, Record: rec, Events: func(s string) { events = append(events, s) }}
	err := r.Execute(context.Background())
	if len(events) == 0 {
		t.Error("no events")
	}
	got, rerr := Read(m)
	if rerr != nil {
		t.Fatal(rerr)
	}
	return got, err
}

func TestSuccess(t *testing.T) {
	p, from, to := upgrade()
	m := &memory{}
	rec, err := run(t, m, p, from, to)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"apply a a:2", "wait a", "apply b b:2", "hook b b:2", "hook c c:2", "apply c c:2", "wait c"}
	if !reflect.DeepEqual(m.log, want) {
		t.Errorf("steps:\n got %v\nwant %v", m.log, want)
	}
	if rec.State != Installed || rec.Pack != "new" || rec.Version != "2" || rec.Saga != nil {
		t.Errorf("record after success: %+v", rec)
	}
	if m.writes != 1+len(p.Steps)+1 {
		t.Errorf("the record must be written before, after every step and at the end: %d writes", m.writes)
	}
}

// c's apply fails: its unit rolls back — c to its previous unit, b's hook
// compensated, b (added) removed — in reverse plan order; a, behind the
// savepoint, stays.
func TestFailureRollsTheUnitBack(t *testing.T) {
	p, from, to := upgrade()
	m := &memory{fail: "apply c c:2"}
	rec, err := run(t, m, p, from, to)
	if err == nil {
		t.Fatal("the saga must fail")
	}
	want := []string{"apply a a:2", "wait a", "apply b b:2", "hook b b:2", "hook c c:2", "apply c c:2",
		"undo apply c c:1", "undo hook b b:2", "undo remove b"}
	if !reflect.DeepEqual(m.log, want) {
		t.Errorf("steps:\n got %v\nwant %v", m.log, want)
	}
	if rec.State != Failed || rec.Saga == nil || rec.Saga.Failed != 6 || rec.Pack != "old" {
		t.Errorf("record after failure: %+v %+v", rec, rec.Saga)
	}
	if got := fmt.Sprint(rec.Saga.RolledBack); got != "[apply c hook b remove b]" {
		t.Errorf("rolled back: %s", got)
	}
}

// --no-rollback b: the rollback stops at b — only c is undone.
func TestRollbackStopsAt(t *testing.T) {
	p, from, to := upgrade()
	p.Units[1].StopsAt = "b"
	m := &memory{fail: "wait c"}
	if _, err := run(t, m, p, from, to); err == nil {
		t.Fatal("the saga must fail")
	}
	want := []string{"apply a a:2", "wait a", "apply b b:2", "hook b b:2", "hook c c:2", "apply c c:2", "wait c", "undo apply c c:1"}
	if !reflect.DeepEqual(m.log, want) {
		t.Errorf("steps:\n got %v\nwant %v", m.log, want)
	}
}

// A failed hook is not compensated — it did not happen — and a failure in
// the first unit leaves nothing of it applied; a hook without compensate is
// left as it is.
func TestFailedHook(t *testing.T) {
	p, from, to := upgrade()
	m := &memory{fail: "hook c c:2"}
	if _, err := run(t, m, p, from, to); err == nil {
		t.Fatal("the saga must fail")
	}
	want := []string{"apply a a:2", "wait a", "apply b b:2", "hook b b:2", "hook c c:2", "undo hook b b:2", "undo remove b"}
	if !reflect.DeepEqual(m.log, want) {
		t.Errorf("steps:\n got %v\nwant %v", m.log, want)
	}
}
