package plan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/sdk/packschema"
	"github.com/ks-tool/pack/target/horchestra"
)

// previous copies the examples into a temp tree and rewinds versions: postgres
// 16.3 → 15.6 (so pg-upgrade's `when` selects), kafka 3.7.1 → 3.6.0, the pack
// 2.1.0 → 2.0.0; zookeeper and jdk untouched.
func previousVersion(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join("..", "examples")
	versions := map[string]string{"postgres": "15.6", "kafka": "3.6.0", "adh": "2.0.0"}
	for _, name := range []string{"adh", "zookeeper", "kafka", "postgres", "jdk"} {
		if err := copyDir(filepath.Join(src, name), filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
		if v, ok := versions[name]; ok {
			pf := filepath.Join(root, name, "pack.yaml")
			raw, _ := os.ReadFile(pf)
			var doc map[string]any
			yaml.Unmarshal(raw, &doc)
			doc["annotations"].(map[string]any)["io.horchestra.version"] = v
			if items, ok := doc["items"].([]any); ok && name != "adh" {
				for _, it := range items {
					im := it.(map[string]any)
					if from, ok := im["from"].(string); ok && strings.HasPrefix(from, "oci://") {
						im["from"] = from[:strings.LastIndex(from, ":")+1] + v
					}
				}
			}
			out, _ := yaml.Marshal(doc)
			os.WriteFile(pf, out, 0o644)
		}
	}
	return filepath.Join(root, "adh")
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

func load(t *testing.T, dir string, v render.Values) (*bundle.Bundle, *render.Release) {
	t.Helper()
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := render.Render(b, v, render.Context{Installation: "prod", Target: horchestra.Context{Installation: "prod", Namespace: "data"}.Target()})
	if err != nil {
		t.Fatal(err)
	}
	return b, out
}

func values(t *testing.T) render.Values {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "examples", "adh", "testdata", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var v render.Values
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The plan from adh 2.0.0 (postgres 15.6, kafka 3.6.0) to 2.1.0 is the golden
// examples/adh/testdata/plan.txt; UPDATE_GOLDEN=1 rewrites it.
func TestPlan_ADHUpgrade_Golden(t *testing.T) {
	v := values(t)
	fromB, fromOut := load(t, previousVersion(t), v)
	toB, toOut := load(t, filepath.Join("..", "examples", "adh"), v)
	p, err := Build(Input{From: fromB, To: toB, FromOut: fromOut, ToOut: toOut})
	if err != nil {
		t.Fatal(err)
	}
	got := p.Text()
	golden := filepath.Join("..", "examples", "adh", "testdata", "plan.txt")
	if len(os.Getenv("UPDATE_GOLDEN")) > 0 {
		os.WriteFile(golden, []byte(got), 0o644)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v — UPDATE_GOLDEN=1 to write it", err)
	}
	if string(want) != got {
		t.Errorf("plan differs from golden:\n%s", got)
	}

	// the properties worth stating
	if p.Direction != "upgrade" || p.Order[0] != "zookeeper" {
		t.Errorf("direction/order: %s %v", p.Direction, p.Order)
	}
	kinds := map[string]string{}
	for _, c := range p.Changes {
		kinds[c.Key] = c.Kind
	}
	if kinds["zookeeper"] != "none" || kinds["kafka"] != "image" || kinds["ranger-db"] != "image" {
		t.Errorf("changes: %v", kinds)
	}
	var stops, pgUpgrades, analyzes int
	for _, s := range p.Steps {
		switch {
		case s.Kind == Stop:
			stops++
		case s.Kind == Hook && s.Name == "pg-upgrade":
			pgUpgrades++
			if !s.Exclusive || len(s.Compensate) == 0 || s.Irreversible {
				t.Errorf("pg-upgrade step: %+v", s)
			}
		case s.Kind == Hook && s.Name == "analyze":
			analyzes++
			if s.Irreversible || !s.Idempotent {
				t.Errorf("analyze must be idempotent, not irreversible: %+v", s)
			}
		}
	}
	if stops != 2 || pgUpgrades != 2 || analyzes != 2 {
		t.Errorf("expected pg-upgrade for both postgres keys: stops %d, pg-upgrade %d, analyze %d", stops, pgUpgrades, analyzes)
	}
	// savepoints from the composition: after zookeeper (unchanged → no unit) and kafka; the two postgres keys end in one unit
	if len(p.Units) != 2 || p.Units[0].Savepoint != "kafka" || p.Units[1].Savepoint != "end" {
		t.Errorf("units: %+v", p.Units)
	}
	if got := strings.Join(p.Units[1].RollbackOrder, ","); got != "hive-db,ranger-db" {
		t.Errorf("rollback order of the last unit must be reverse plan order: %s", got)
	}
}

func TestPlan_PolicyAndRefusals(t *testing.T) {
	v := values(t)
	fromB, fromOut := load(t, previousVersion(t), v)
	toB, toOut := load(t, filepath.Join("..", "examples", "adh"), v)
	in := Input{From: fromB, To: toB, FromOut: fromOut, ToOut: toOut}

	in.Policy = Policy{Savepoints: "none"}
	p, _ := Build(in)
	if len(p.Units) != 1 {
		t.Errorf("savepoints none → one unit: %+v", p.Units)
	}
	in.Policy = Policy{Savepoints: "auto"}
	p, _ = Build(in)
	if len(p.Units) != 3 {
		t.Errorf("savepoints auto → one unit per changed key: %+v", p.Units)
	}
	in.Policy = Policy{NoRollback: []string{"ranger-db"}}
	p, _ = Build(in)
	if last := p.Units[len(p.Units)-1]; last.StopsAt != "ranger-db" || last.RollbackOrder[0] != "hive-db" {
		t.Errorf("--no-rollback ranger-db: hive-db rolls back, the rollback stops at ranger-db: %+v", last)
	}
	in.Policy = Policy{NoRollbackAll: true}
	p, _ = Build(in)
	if p.Units[0].StopsAt != "*" {
		t.Errorf("--no-rollback all: %+v", p.Units[0])
	}
	in.Policy = Policy{Savepoints: "nope"}
	if _, err := Build(in); err == nil {
		t.Error("an unknown savepoint key must be refused")
	}
	// an irreversible hook: reported in the plan, refused under --strict
	irr := t.TempDir()
	if err := copyDir(filepath.Join("..", "examples"), irr); err != nil {
		t.Fatal(err)
	}
	sp := filepath.Join(irr, "postgres", "schema.json")
	raw, _ := os.ReadFile(sp)
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, h := range doc["x-pack"].(map[string]any)["hooks"].([]any) {
		delete(h.(map[string]any), "compensate")
	}
	out, _ := json.Marshal(doc)
	os.WriteFile(sp, out, 0o644)
	irrB, irrOut := load(t, filepath.Join(irr, "adh"), v)
	p, err := Build(Input{From: fromB, To: irrB, FromOut: fromOut, ToOut: irrOut})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Text(), "IRREVERSIBLE") {
		t.Errorf("an irreversible hook must be printed as such:\n%s", p.Text())
	}
	if _, err := Build(Input{From: fromB, To: irrB, FromOut: fromOut, ToOut: irrOut, Policy: Policy{Strict: true}}); err == nil {
		t.Error("--strict must refuse an irreversible step")
	}

	// the same version both sides: nothing to do
	sameB, sameOut := load(t, filepath.Join("..", "examples", "adh"), v)
	p, err = Build(Input{From: sameB, To: toB, FromOut: sameOut, ToOut: toOut})
	if err != nil || len(p.Steps) != 0 {
		t.Errorf("no change → no steps: %v %+v", err, p.Steps)
	}
}

// The plan of a first installation: every key is added, the install hooks run
// around the first apply, nothing is stopped, the composition's savepoints
// still cut the units. Golden examples/adh/testdata/install-plan.txt.
func TestPlan_ADHInstall_Golden(t *testing.T) {
	v := values(t)
	toB, toOut := load(t, filepath.Join("..", "examples", "adh"), v)
	p, err := Build(Input{To: toB, ToOut: toOut})
	if err != nil {
		t.Fatal(err)
	}
	got := p.Text()
	golden := filepath.Join("..", "examples", "adh", "testdata", "install-plan.txt")
	if len(os.Getenv("UPDATE_GOLDEN")) > 0 {
		os.WriteFile(golden, []byte(got), 0o644)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v — UPDATE_GOLDEN=1 to write it", err)
	}
	if string(want) != got {
		t.Errorf("plan differs from golden:\n%s", got)
	}
	if p.Direction != "install" || len(p.From) > 0 {
		t.Errorf("direction %q from %q", p.Direction, p.From)
	}
	for _, c := range p.Changes {
		if c.Kind != "added" || c.Direction != "install" {
			t.Errorf("every key is installed: %+v", c)
		}
	}
	var inits, upgrades, stops int
	for _, s := range p.Steps {
		switch {
		case s.Kind == Stop:
			stops++
		case s.Kind == Hook && s.Name == "init-extensions":
			inits++
		case s.Kind == Hook && (s.Name == "pg-upgrade" || s.Name == "analyze"):
			upgrades++
		case s.Kind == Apply && s.Compensate != "remove (PVs stay)":
			t.Errorf("undoing an install is a removal: %+v", s)
		}
	}
	if inits != 2 || upgrades != 0 || stops != 0 {
		t.Errorf("install hooks for both postgres keys, no upgrade hooks, no stops: %d %d %d", inits, upgrades, stops)
	}
	// savepoints [zookeeper, kafka] from the composition: three units
	if len(p.Units) != 3 || p.Units[0].Savepoint != "zookeeper" || p.Units[1].Savepoint != "kafka" || p.Units[2].Savepoint != "end" {
		t.Errorf("units: %+v", p.Units)
	}
}

// A key the old version did not have is installed inside an upgrade: its
// install hooks are selected, not the upgrade ones.
func TestSelectHooks_AddedKeyInstalls(t *testing.T) {
	comp := &bundle.Component{Root: &packschema.Root{Hooks: []packschema.Hook{
		{Name: "bootstrap", Phase: "postInstall", Command: []string{"x"}},
		{Name: "migrate", Phase: "preUpgrade", Command: []string{"x"}},
	}}}
	c := Change{Key: "k", Kind: "added", Direction: "install", To: "1.0.0"}
	p := &Plan{}
	pre, post := selectHooks(comp, c, c.Direction, p)
	if len(pre) != 0 || len(post) != 1 || post[0].Name != "bootstrap" || len(p.Warnings) != 0 {
		t.Errorf("pre %v post %v warnings %v", pre, post, p.Warnings)
	}
	c = Change{Key: "k", Kind: "image", Direction: "upgrade", From: "0.9.0", To: "1.0.0"}
	pre, post = selectHooks(comp, c, c.Direction, p)
	if len(pre) != 1 || pre[0].Name != "migrate" || len(post) != 0 {
		t.Errorf("upgrade: pre %v post %v", pre, post)
	}
}

// The same version with only a config value changed: the target names its
// config objects stably, so the change is in the files, never in a name —
// kafka is "config", everything else "none".
func TestPlan_ConfigOnlyChange(t *testing.T) {
	adh := filepath.Join("..", "examples", "adh")
	fromB, fromOut := load(t, adh, values(t))
	v := values(t)
	v.Values["kafka"].(map[string]any)["conf"] = map[string]any{"min.insync.replicas": 3}
	toB, toOut := load(t, adh, v)
	p, err := Build(Input{From: fromB, To: toB, FromOut: fromOut, ToOut: toOut})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, c := range p.Changes {
		kinds[c.Key] = c.Kind
	}
	if kinds["kafka"] != "config" || kinds["zookeeper"] != "none" || kinds["ranger-db"] != "none" {
		t.Errorf("changes: %v", kinds)
	}
}

// A change that reaches no file — the platform, where and as how many a key
// runs, its resources — is a change of the key all the same: "config", an
// apply without hooks.
func TestPlan_PlatformOnlyChange(t *testing.T) {
	adh := filepath.Join("..", "examples", "adh")
	fromB, fromOut := load(t, adh, values(t))
	v := values(t)
	v.Values["ranger-db"].(map[string]any)["platform"] = map[string]any{"resources": map[string]any{"limits": map[string]any{"cpu": "3"}}}
	toB, toOut := load(t, adh, v)
	p, err := Build(Input{From: fromB, To: toB, FromOut: fromOut, ToOut: toOut})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, c := range p.Changes {
		kinds[c.Key] = c.Kind
	}
	if kinds["ranger-db"] != "config" || kinds["kafka"] != "none" {
		t.Errorf("changes: %v", kinds)
	}
}

// Undoing postgres's upgrade runs the INSTALLED version's downgrade hooks:
// a preDowngrade hook of postgres 15.6, its `when` matched against the
// reverse transition (16.3 → 15.6), its image the version being restored.
func TestPlan_RollbackHooks(t *testing.T) {
	prev := previousVersion(t)
	sf := filepath.Join(filepath.Dir(prev), "postgres", "schema.json")
	raw, err := os.ReadFile(sf)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	xp := doc["x-pack"].(map[string]any)
	xp["hooks"] = append(xp["hooks"].([]any), map[string]any{
		"name": "dump", "phase": "preDowngrade", "command": []any{"pg_dumpall"}, "exclusive": true,
		"when": map[string]any{"from": ">=16", "to": "<16"},
	})
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(sf, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	v := values(t)
	fromB, fromOut := load(t, prev, v)
	toB, toOut := load(t, filepath.Join("..", "examples", "adh"), v)
	p, err := Build(Input{From: fromB, To: toB, FromOut: fromOut, ToOut: toOut})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ranger-db", "hive-db"} {
		kr, ok := p.Rollback[key]
		if !ok || !kr.Stop || len(kr.Pre) != 1 || kr.Pre[0].Name != "dump" || !strings.HasSuffix(kr.Pre[0].Image, ":15.6") {
			t.Errorf("%s: rollback %+v", key, kr)
		}
	}
	if _, ok := p.Rollback["kafka"]; ok {
		t.Error("kafka has no downgrade hooks: nothing beyond the previous unit")
	}
	if !strings.Contains(p.Text(), "rollback of ranger-db: stop → preDowngrade dump → apply the previous unit") {
		t.Errorf("the plan must show how the upgrade is undone:\n%s", p.Text())
	}
}
