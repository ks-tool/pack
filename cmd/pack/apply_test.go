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

// pack apply end to end, each side its own process: pack and the test
// provider "fake", which writes down every step it is asked to perform and
// keeps pack's record in a directory.
func TestApply(t *testing.T) {
	if testing.Short() {
		t.Skip("builds pack and a provider")
	}
	bin := t.TempDir()
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go")
	for pkg, out := range map[string]string{".": "pack", "../../provider/testdata/fake": "pack-provider-fake"} {
		if b, err := exec.Command(gobin, "build", "-o", filepath.Join(bin, out), pkg).CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", pkg, err, b)
		}
	}
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
		cmd := exec.Command(filepath.Join(bin, "pack"), "apply", "--pack", adh, "--values", values, "--name", "prod", "--target", "fake")
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

	// a saga that did not finish stops the next one, until --force
	if out, err := apply(values); err == nil || !strings.Contains(out, "did not finish") {
		t.Errorf("an unfinished saga must be reported, not planned over: %v\n%s", err, out)
	}
}
