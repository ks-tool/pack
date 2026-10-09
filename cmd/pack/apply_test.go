package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ks-tool/pack/saga"
)

// buildWithFake builds pack and the test provider side by side.
func buildWithFake(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go")
	for pkg, out := range map[string]string{".": "pack", "../../provider/testdata/fake": "pack-provider-fake"} {
		if b, err := exec.Command(gobin, "build", "-o", filepath.Join(bin, out), pkg).CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", pkg, err, b)
		}
	}
	return bin
}

// pack apply end to end, each side its own process: pack and the test
// provider "fake", which writes down every step it is asked to perform and
// keeps pack's record in a directory.
func TestApply(t *testing.T) {
	if testing.Short() {
		t.Skip("builds pack and a provider")
	}
	bin := buildWithFake(t)
	state := t.TempDir()
	adh := filepath.Join("..", "..", "examples", "adh")
	values := filepath.Join(adh, "testdata", "values.yaml")
	raw, err := os.ReadFile(values)
	if err != nil {
		t.Fatal(err)
	}
	changed := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(changed, []byte(strings.Replace(string(raw), "min.insync.replicas: 2", "min.insync.replicas: 3", 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	apply := func(values string, env ...string) (string, error) {
		args := []string{"apply", "--pack", adh, "--values", values, "--name", "prod", "--target", "fake"}
		for len(env) > 0 && strings.HasPrefix(env[0], "--") {
			args, env = append(args, env[0]), env[1:]
		}
		cmd := exec.Command(filepath.Join(bin, "pack"), args...)
		cmd.Env = append(append(os.Environ(), "PACK_FAKE_DIR="+state, "PACK_PROVIDER_PATH="), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	journal := func() []string {
		b, _ := os.ReadFile(filepath.Join(state, "log"))
		_ = os.Remove(filepath.Join(state, "log"))
		if len(b) == 0 {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	record := func() saga.Record {
		var r saga.Record
		b, err := os.ReadFile(filepath.Join(state, "record.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}

	// a first installation: every key applied, in order
	if out, err := apply(values); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	j := strings.Join(journal(), "; ")
	if !strings.HasPrefix(j, "apply zookeeper") || !strings.Contains(j, "apply kafka") {
		t.Errorf("install performed: %s", j)
	}
	if r := record(); r.State != saga.Installed || r.Version != "2.1.0" || !filepath.IsAbs(r.Pack) {
		t.Errorf("record after install: %+v", r)
	}

	// the same again: nothing to do
	if out, err := apply(values); err != nil {
		t.Fatalf("reapply: %v\n%s", err, out)
	}
	if j := journal(); len(j) != 0 {
		t.Errorf("an unchanged installation must perform nothing: %v", j)
	}

	// a config-only change: kafka alone is applied
	if out, err := apply(changed); err != nil {
		t.Fatalf("config change: %v\n%s", err, out)
	}
	if j := strings.Join(journal(), "; "); !strings.HasPrefix(j, "apply kafka") || strings.Contains(j, "zookeeper") {
		t.Errorf("a config change of kafka performed: %s", j)
	}

	// back to the old value, with kafka's apply failing: kafka goes back to
	// what was installed, and the record says the saga failed
	out, err := apply(values, "PACK_FAKE_FAIL=apply kafka")
	if err == nil {
		t.Fatalf("the failing apply must fail:\n%s", out)
	}
	if j := strings.Join(journal(), "; "); !strings.Contains(j, "undo apply kafka") {
		t.Errorf("the failed apply must be undone: %s\n%s", j, out)
	}
	if r := record(); r.State != saga.Failed || r.Saga == nil || !strings.Contains(r.Saga.Error, "fake failure") {
		t.Errorf("record after a failure: %+v", r)
	}

	// another command over the unfinished saga is refused …
	if out, err := apply(changed); err == nil || !strings.Contains(out, "did not finish") {
		t.Errorf("another saga over an unfinished one must be refused: %v\n%s", err, out)
	}
	// … the same one continues it from the savepoint: kafka's unit again
	if out, err := apply(values); err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if j := strings.Join(journal(), "; "); !strings.HasPrefix(j, "apply kafka") || strings.Contains(j, "zookeeper") {
		t.Errorf("the resumed saga performed: %s", j)
	}
	if r := record(); r.State != saga.Installed || r.Saga != nil {
		t.Errorf("record after the resumed saga: %+v", r)
	}

	// --from-start over an unfinished saga starts a new one from its first step
	if out, err := apply(changed, "PACK_FAKE_FAIL=wait kafka"); err == nil {
		t.Fatalf("the failing saga must fail:\n%s", out)
	}
	journal()
	if out, err := apply(changed, "--from-start"); err != nil {
		t.Fatalf("from start: %v\n%s", err, out)
	}
	if j := strings.Join(journal(), "; "); !strings.HasPrefix(j, "apply kafka") {
		t.Errorf("--from-start performed: %s", j)
	}
}

// pack rollback end to end: a finished upgrade goes back to the version it
// replaced; an unfinished first installation is undone whole, the units
// behind its savepoints too.
func TestRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("builds pack and a provider")
	}
	bin := buildWithFake(t)
	adh := filepath.Join("..", "..", "examples", "adh")
	values := filepath.Join(adh, "testdata", "values.yaml")
	raw, _ := os.ReadFile(values)
	changed := filepath.Join(t.TempDir(), "values.yaml")
	_ = os.WriteFile(changed, []byte(strings.Replace(string(raw), "min.insync.replicas: 2", "min.insync.replicas: 3", 1)), 0o600)

	state := t.TempDir()
	pack := func(env []string, args ...string) (string, error) {
		cmd := exec.Command(filepath.Join(bin, "pack"), append(args, "--name", "prod", "--target", "fake")...)
		cmd.Env = append(append(os.Environ(), "PACK_FAKE_DIR="+state, "PACK_PROVIDER_PATH="), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	journal := func() string {
		b, _ := os.ReadFile(filepath.Join(state, "log"))
		_ = os.Remove(filepath.Join(state, "log"))
		return strings.Join(strings.Split(strings.TrimSpace(string(b)), "\n"), "; ")
	}

	if out, err := pack(nil, "apply", "--pack", adh, "--values", values); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if out, err := pack(nil, "rollback"); err == nil || !strings.Contains(out, "no previous version") {
		t.Errorf("a first installation has nothing to roll back to: %v\n%s", err, out)
	}
	if out, err := pack(nil, "apply", "--pack", adh, "--values", changed); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	journal()
	// finished: a saga back to the version it replaced — kafka's config again
	if out, err := pack(nil, "rollback"); err != nil {
		t.Fatalf("rollback: %v\n%s", err, out)
	}
	if j := journal(); !strings.HasPrefix(j, "apply kafka") || strings.Contains(j, "zookeeper") {
		t.Errorf("rollback of the upgrade performed: %s", j)
	}

	// unfinished first installation: kafka's unit failed and was rolled
	// back; rollback takes zookeeper's unit back too — nothing is installed
	state = t.TempDir()
	if out, err := pack([]string{"PACK_FAKE_FAIL=apply kafka"}, "apply", "--pack", adh, "--values", values); err == nil {
		t.Fatalf("the failing install must fail:\n%s", out)
	}
	journal()
	out, err := pack(nil, "rollback")
	if err != nil {
		t.Fatalf("rollback: %v\n%s", err, out)
	}
	if j := journal(); !strings.Contains(j, "undo remove zookeeper") {
		t.Errorf("rollback of the unfinished install performed: %s\n%s", j, out)
	}
	if out, err := pack(nil, "rollback"); err == nil || !strings.Contains(out, "nothing is installed") {
		t.Errorf("after the undo nothing is installed: %v\n%s", err, out)
	}
}
