package saga

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ks-tool/pack/plan"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

// memory is a target that performs nothing: it logs what it was asked, fails
// the step named in fail, and keeps the record in memory. during, when set,
// hears each step as it runs — an interrupt arrives there — and a step whose
// context is done by then fails as cut short.
type memory struct {
	log    []string
	fail   string // "<kind> <key>"
	record []byte
	writes int
	during func(what string)
}

func (m *memory) ReadInstalled() ([]byte, bool, error) { return m.record, m.record != nil, nil }
func (m *memory) WriteInstalled(r []byte) error {
	m.record = r
	m.writes++
	if m.during != nil {
		m.during(fmt.Sprintf("write %d", m.writes))
	}
	return nil
}
func (m *memory) Execute(ctx context.Context, s target.Step, events func(string)) error {
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
	if m.during != nil {
		m.during(what)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
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
	if got := fmt.Sprint(rec.Saga.RolledBack); got != "[apply c hook b init remove b]" {
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

// resumeWith runs the same saga again over the record m keeps.
func resumeWith(t *testing.T, m *memory, p *plan.Plan, from, to *render.Release) (*Record, error) {
	t.Helper()
	rec, err := Read(m)
	if err != nil || rec == nil {
		t.Fatalf("no record to resume: %v", err)
	}
	m.log = nil
	r := &Run{Plan: p, From: from, To: to, Exec: m, Record: rec}
	err = r.Execute(context.Background())
	got, _ := Read(m)
	return got, err
}

// A saga that failed in unit 2 and was rolled back continues from unit 2's
// first step: unit 1, behind the savepoint, is not performed again.
func TestResumeAfterFailure(t *testing.T) {
	p, from, to := upgrade()
	m := &memory{fail: "apply c c:2"}
	if _, err := run(t, m, p, from, to); err == nil {
		t.Fatal("the saga must fail")
	}
	m.fail = ""
	rec, err := resumeWith(t, m, p, from, to)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"apply b b:2", "hook b b:2", "hook c c:2", "apply c c:2", "wait c"}
	if !reflect.DeepEqual(m.log, want) {
		t.Errorf("resumed steps:\n got %v\nwant %v", m.log, want)
	}
	if rec.State != Installed || rec.Version != "2" || rec.Saga != nil {
		t.Errorf("record after the resumed saga: %+v", rec)
	}
}

// A saga cut short inside unit 2 — no rollback ran — has the unit's
// performed steps undone first, then the unit runs from its start.
func TestResumeAfterCrash(t *testing.T) {
	p, from, to := upgrade()
	m := &memory{}
	rec := &Record{Pack: "old", Version: "1", State: InProgress,
		Saga: &Progress{Pack: "new", Version: "2", Done: []int{1, 2, 3, 4}}}
	if err := Write(m, rec); err != nil {
		t.Fatal(err)
	}
	got, err := resumeWith(t, m, p, from, to)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"undo hook b b:2", "undo remove b",
		"apply b b:2", "hook b b:2", "hook c c:2", "apply c c:2", "wait c"}
	if !reflect.DeepEqual(m.log, want) {
		t.Errorf("resumed steps:\n got %v\nwant %v", m.log, want)
	}
	if got.State != Installed {
		t.Errorf("record: %+v", got)
	}
}

// What the rollback policy left in place (--no-rollback b) is undone when
// the saga continues: the unit is all or nothing.
func TestResumeUndoesWhatThePolicyLeft(t *testing.T) {
	p, from, to := upgrade()
	p.Units[1].StopsAt = "b"
	m := &memory{fail: "wait c"}
	if _, err := run(t, m, p, from, to); err == nil {
		t.Fatal("the saga must fail")
	}
	m.fail = ""
	if _, err := resumeWith(t, m, p, from, to); err != nil {
		t.Fatal(err)
	}
	want := []string{"undo hook b b:2", "undo remove b",
		"apply b b:2", "hook b b:2", "hook c c:2", "apply c c:2", "wait c"}
	if !reflect.DeepEqual(m.log, want) {
		t.Errorf("resumed steps:\n got %v\nwant %v", m.log, want)
	}
}

// Undoing c's apply — c changes version — runs the installed version's
// downgrade hooks around applying the previous unit: stop (the pre hook is
// exclusive), preDowngrade, apply c:1, postDowngrade; c's migrate hook has no
// compensate and is left as it is.
func TestRollbackRunsDowngradeHooks(t *testing.T) {
	p, from, to := upgrade()
	p.Rollback = map[string]plan.KeyRollback{"c": {
		Stop: true,
		Pre:  []plan.Step{{Key: "c", Kind: plan.Hook, Name: "unmigrate", Phase: "preDowngrade", Exclusive: true, Image: "c:1", Command: []string{"unmigrate"}}},
		Post: []plan.Step{{Key: "c", Kind: plan.Hook, Name: "reindex", Phase: "postDowngrade", Image: "c:1", Command: []string{"reindex"}}},
	}}
	m := &memory{fail: "wait c"}
	rec, err := run(t, m, p, from, to)
	if err == nil {
		t.Fatal("the saga must fail")
	}
	want := []string{"apply a a:2", "wait a", "apply b b:2", "hook b b:2", "hook c c:2", "apply c c:2", "wait c",
		"undo stop c", "undo hook c c:1", "undo apply c c:1", "undo hook c c:1", "undo hook b b:2", "undo remove b"}
	if !reflect.DeepEqual(m.log, want) {
		t.Errorf("steps:\n got %v\nwant %v", m.log, want)
	}
	if got := fmt.Sprint(rec.Saga.RolledBack); got != "[stop c hook c unmigrate apply c hook c reindex hook b init remove b]" {
		t.Errorf("rolled back: %s", got)
	}
}

// pack rollback of a saga that failed in unit 2: unit 2 was rolled back
// already; unit 1, behind its savepoint, is undone too — the installation
// is as before the saga.
func TestUndoUnfinished(t *testing.T) {
	p, from, to := upgrade()
	m := &memory{fail: "apply c c:2"}
	if _, err := run(t, m, p, from, to); err == nil {
		t.Fatal("the saga must fail")
	}
	rec, _ := Read(m)
	m.log, m.fail = nil, ""
	r := &Run{Plan: p, From: from, To: to, Exec: m, Record: rec}
	if err := r.Undo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"undo apply a a:1"}; !reflect.DeepEqual(m.log, want) {
		t.Errorf("undone:\n got %v\nwant %v", m.log, want)
	}
	got, _ := Read(m)
	if got.State != Installed || got.Pack != "old" || got.Version != "1" || got.Saga != nil {
		t.Errorf("record after undo: %+v", got)
	}
}

// A finished saga records what it replaced: where pack rollback goes.
func TestPreviousIsRecorded(t *testing.T) {
	p, from, to := upgrade()
	m := &memory{}
	rec, err := run(t, m, p, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Previous == nil || rec.Previous.Pack != "old" || rec.Previous.Version != "1" {
		t.Errorf("previous: %+v", rec.Previous)
	}
}

// A finished saga records the target's half of each key's platform as the
// version it installed resolved it, and the one it replaced as Previous's:
// rendered again, either is what was installed. A change of nothing but
// where a key runs is a version of its own, to roll back to.
func TestPlatformIsRecorded(t *testing.T) {
	p, from, to := upgrade()
	to.Platforms = map[string]map[string]any{"a": {"hosts": []any{"h2"}}}
	m := &memory{}
	rec := &Record{Pack: "new", Name: "x", Version: "2", State: Installed,
		Platform: map[string]map[string]any{"a": {"hosts": []any{"h1"}}},
		Saga:     &Progress{Pack: "new", Version: "2"}}
	if err := (&Run{Plan: p, From: from, To: to, Exec: m, Record: rec}).Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := Read(m)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got.Platform) != "map[a:map[hosts:[h2]]]" {
		t.Errorf("platform: %v", got.Platform)
	}
	if got.Previous == nil || fmt.Sprint(got.Previous.Platform) != "map[a:map[hosts:[h1]]]" {
		t.Errorf("previous: %+v", got.Previous)
	}
}

// interrupted runs the saga and interrupts it where the step at soft runs,
// then — the second interrupt — where the step at hard runs.
func interrupted(t *testing.T, m *memory, p *plan.Plan, from, to *render.Release, soft, hard string) (*Record, error) {
	t.Helper()
	interrupt, cancelSoft := context.WithCancel(context.Background())
	ctx, cancelHard := context.WithCancel(context.Background())
	defer cancelSoft()
	defer cancelHard()
	m.during = func(what string) {
		switch what {
		case soft:
			cancelSoft()
		case hard:
			cancelHard()
		}
	}
	rec := &Record{Pack: "old", Name: "x", Version: "1", State: Installed, Saga: &Progress{Pack: "new", Version: "2"}}
	r := &Run{Plan: p, From: from, To: to, Exec: m, Record: rec, Interrupt: interrupt}
	err := r.Execute(ctx)
	m.during = nil
	got, rerr := Read(m)
	if rerr != nil {
		t.Fatal(rerr)
	}
	return got, err
}

// The first interrupt cuts c's apply short and fails it: its unit rolls back
// as for any failure.
func TestInterruptRollsTheUnitBack(t *testing.T) {
	p, from, to := upgrade()
	m := &memory{}
	rec, err := interrupted(t, m, p, from, to, "apply c c:2", "")
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("the saga must fail as interrupted: %v", err)
	}
	want := []string{"apply a a:2", "wait a", "apply b b:2", "hook b b:2", "hook c c:2", "apply c c:2",
		"undo apply c c:1", "undo hook b b:2", "undo remove b"}
	if !reflect.DeepEqual(m.log, want) {
		t.Errorf("steps:\n got %v\nwant %v", m.log, want)
	}
	if rec.State != Failed || rec.Saga.Failed != 6 || rec.Saga.Error != "interrupted" ||
		!reflect.DeepEqual(rec.Saga.Done, []int{1, 2}) || rec.Saga.Current != 0 {
		t.Errorf("record after the interrupt: %+v", rec.Saga)
	}
}

// An interrupt between steps — a's wait recorded done, b's apply not yet
// begun — keeps the next one from starting: unit 2 has nothing to undo.
func TestInterruptBetweenSteps(t *testing.T) {
	p, from, to := upgrade()
	m := &memory{}
	rec, err := interrupted(t, m, p, from, to, "write 3", "")
	if !errors.Is(err, ErrInterrupted) || !strings.HasSuffix(err.Error(), "rolled back: nothing") {
		t.Fatalf("the saga must fail as interrupted, nothing rolled back: %v", err)
	}
	if want := []string{"apply a a:2", "wait a"}; !reflect.DeepEqual(m.log, want) {
		t.Errorf("steps:\n got %v\nwant %v", m.log, want)
	}
	if rec.Saga.Failed != 3 || !reflect.DeepEqual(rec.Saga.Done, []int{1, 2}) || rec.Saga.Current != 0 {
		t.Errorf("record: %+v", rec.Saga)
	}
}

// A wait cut short fails, and its unit — a's apply — is undone.
func TestInterruptedWait(t *testing.T) {
	p, from, to := upgrade()
	m := &memory{}
	rec, err := interrupted(t, m, p, from, to, "wait a", "")
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("the saga must fail as interrupted: %v", err)
	}
	if want := []string{"apply a a:2", "wait a", "undo apply a a:1"}; !reflect.DeepEqual(m.log, want) {
		t.Errorf("steps:\n got %v\nwant %v", m.log, want)
	}
	if rec.Saga.Failed != 2 || len(rec.Saga.Done) != 0 {
		t.Errorf("record: %+v", rec.Saga)
	}
}

// The second interrupt stops the rollback at once. The record says what is
// in effect and which apply was under way; the same apply again undoes them
// and runs the unit from its start.
func TestSecondInterruptStops(t *testing.T) {
	p, from, to := upgrade()
	m := &memory{}
	rec, err := interrupted(t, m, p, from, to, "apply c c:2", "undo apply c c:1")
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("the saga must stop as interrupted: %v", err)
	}
	want := []string{"apply a a:2", "wait a", "apply b b:2", "hook b b:2", "hook c c:2", "apply c c:2", "undo apply c c:1"}
	if !reflect.DeepEqual(m.log, want) {
		t.Errorf("steps:\n got %v\nwant %v", m.log, want)
	}
	if !rec.Unfinished() || !reflect.DeepEqual(rec.Saga.Done, []int{1, 2, 3, 4, 5}) || rec.Saga.Current != 6 {
		t.Fatalf("record after the second interrupt: %s %+v", rec.State, rec.Saga)
	}

	got, err := resumeWith(t, m, p, from, to)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"undo apply c c:1", "undo hook b b:2", "undo remove b",
		"apply b b:2", "hook b b:2", "hook c c:2", "apply c c:2", "wait c"}
	if !reflect.DeepEqual(m.log, want) {
		t.Errorf("resumed steps:\n got %v\nwant %v", m.log, want)
	}
	if got.State != Installed || got.Version != "2" {
		t.Errorf("record after the resumed saga: %+v", got)
	}
}

// pack rollback after the second interrupt takes everything back, the apply
// that was under way included.
func TestUndoAfterSecondInterrupt(t *testing.T) {
	p, from, to := upgrade()
	m := &memory{}
	if _, err := interrupted(t, m, p, from, to, "apply c c:2", "undo apply c c:1"); err == nil {
		t.Fatal("the saga must stop")
	}
	rec, _ := Read(m)
	m.log = nil
	r := &Run{Plan: p, From: from, To: to, Exec: m, Record: rec}
	if err := r.Undo(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"undo apply c c:1", "undo hook b b:2", "undo remove b", "undo apply a a:1"}
	if !reflect.DeepEqual(m.log, want) {
		t.Errorf("undone:\n got %v\nwant %v", m.log, want)
	}
	if got, _ := Read(m); got.State != Installed || got.Version != "1" || got.Saga != nil {
		t.Errorf("record after undo: %+v", got)
	}
}

// A process killed during an apply: the record names it under way, and the
// saga continued undoes it with the rest of its unit.
func TestResumeAfterCrashDuringApply(t *testing.T) {
	p, from, to := upgrade()
	m := &memory{}
	rec := &Record{Pack: "old", Version: "1", State: InProgress,
		Saga: &Progress{Pack: "new", Version: "2", Done: []int{1, 2, 3, 4, 5}, Current: 6}}
	if err := Write(m, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := resumeWith(t, m, p, from, to); err != nil {
		t.Fatal(err)
	}
	want := []string{"undo apply c c:1", "undo hook b b:2", "undo remove b",
		"apply b b:2", "hook b b:2", "hook c c:2", "apply c c:2", "wait c"}
	if !reflect.DeepEqual(m.log, want) {
		t.Errorf("resumed steps:\n got %v\nwant %v", m.log, want)
	}
}

// What a run rolled back is its own: the error and the record of a second
// failure do not carry the first one's rollback.
func TestRolledBackIsThisRun(t *testing.T) {
	p, from, to := upgrade()
	m := &memory{fail: "apply c c:2"}
	if _, err := run(t, m, p, from, to); err == nil {
		t.Fatal("the saga must fail")
	}
	_, err := resumeWith(t, m, p, from, to)
	if err == nil {
		t.Fatal("the saga must fail again")
	}
	if want := "rolled back: apply c, hook b init, remove b"; !strings.HasSuffix(err.Error(), want) {
		t.Errorf("error: %v\nwant it to end in %q", err, want)
	}
	rec, _ := Read(m)
	if got := fmt.Sprint(rec.Saga.RolledBack); got != "[apply c hook b init remove b]" {
		t.Errorf("rolled back: %s", got)
	}
}
