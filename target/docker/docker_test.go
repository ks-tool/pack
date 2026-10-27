package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/environment"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

// adh renders the adh example into its Docker environment.
func adh(t *testing.T) (*Installation, *render.Release) {
	t.Helper()
	root := filepath.Join("..", "..", "examples", "adh")
	env, err := environment.Load(context.Background(), filepath.Join(root, "envs", "docker.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := Target{}.Configure("prod", target.Settings(env.Settings))
	if err != nil {
		t.Fatal(err)
	}
	b, err := bundle.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "testdata", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var v render.Values
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	rel, err := render.Render(b, v, render.Context{Installation: "prod", Target: inst})
	if err != nil {
		t.Fatal(err)
	}
	return inst.(*Installation), rel
}

// The adh composition in its Docker environment is this target's golden
// output: compose.yaml and the report under testdata/adh/expected.
// UPDATE_GOLDEN=1 rewrites them.
func TestConvertADH_Golden(t *testing.T) {
	inst, rel := adh(t)
	out, err := inst.Convert(rel, nil)
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join("testdata", "adh", "expected")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		_ = os.RemoveAll(expected)
		for _, f := range out.Files {
			p := filepath.Join(expected, filepath.FromSlash(f.Path))
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, f.Data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, f := range out.Files {
		want, err := os.ReadFile(filepath.Join(expected, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatalf("%s: %v — UPDATE_GOLDEN=1 to write it", f.Path, err)
		}
		if string(want) != string(f.Data) {
			t.Errorf("%s differs from golden:\n%s", f.Path, f.Data)
		}
	}
}

// What the engine runs, stated: a container per replica answering to the
// key's name, a per-instance file as each container's own config, a library
// as an image mount, a placeholder as a compose variable.
func TestConvertADH_Properties(t *testing.T) {
	inst, rel := adh(t)
	out, err := inst.Convert(rel, nil)
	if err != nil {
		t.Fatal(err)
	}
	compose := string(out.Files[0].Data)
	for _, want := range []string{
		"container_name: prod-zookeeper-2", "- prod-zookeeper", "name: prod",
		"type: image", "source: images/jdk:21.0.4",
		"prod-zookeeper-0:2181", "${PACK_SECRET_PROD_KAFKA_SASL_PASSWORD}",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose lacks %q", want)
		}
	}
	// zookeeper's myid differs per container
	var doc struct {
		Configs map[string]struct {
			Content string `json:"content"`
		} `json:"configs"`
	}
	if err := yaml.Unmarshal(out.Files[0].Data, &doc); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for name, c := range doc.Configs {
		if strings.HasPrefix(name, "zookeeper-") && strings.HasSuffix(name, "-myid") {
			ids[strings.TrimSpace(c.Content)] = true
		}
	}
	if len(ids) != 3 {
		t.Errorf("myid per container: %v", ids)
	}
}

func TestPublish(t *testing.T) {
	inst := &Installation{installation: "dev", network: "dev", settings: target.Settings{"platform_pg_publish_pg": "15432"}}
	res, err := inst.Resolve("pg", "single", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Address != "dev-pg" || res.Platform["publish"].(map[string]any)["pg"] != int64(15432) {
		t.Errorf("%+v", res)
	}
	if _, err := inst.Resolve("pg", "replicas", map[string]any{"replicas": 2}); err == nil {
		t.Error("two replicas cannot publish one host port")
	}
}

func TestQuantity(t *testing.T) {
	for s, want := range map[string]float64{"500m": 0.5, "2": 2, "512Mi": 512 << 20, "1G": 1e9, "1.5Gi": 1.5 * (1 << 30)} {
		if got, err := quantity(s); err != nil || got != want {
			t.Errorf("%s: %v %v", s, got, err)
		}
	}
	if _, err := quantity("lots"); err == nil {
		t.Error("lots is no quantity")
	}
}

func TestInterpolate(t *testing.T) {
	got := interpolate("password=${secret:pg/password}\nhome=$HOME\n")
	if got != "password=${PACK_SECRET_PG_PASSWORD}\nhome=$$HOME\n" {
		t.Errorf("%q", got)
	}
	s, err := substitute("password=${secret:pg/password}\n", func(ref string) (string, error) { return "s3cret", nil })
	if err != nil || s != "password=s3cret\n" {
		t.Errorf("%q %v", s, err)
	}
}

// A state file keeps each installation's record and credentials apart, and
// the engine is never asked.
func TestFileState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "docker.json")
	open := func(name string) *Installation {
		inst, err := Target{}.Configure(name, target.Settings{"state": path, "docker": "/nonexistent/docker"})
		if err != nil {
			t.Fatal(err)
		}
		return inst.(*Installation)
	}
	prod, dev := open("prod"), open("dev")
	if _, exists, err := prod.ReadInstalled(); err != nil || exists {
		t.Fatalf("nothing installed yet: %v %v", exists, err)
	}
	if err := prod.WriteInstalled([]byte(`{"name":"prod","state":"installed"}`)); err != nil {
		t.Fatal(err)
	}
	if err := dev.WriteInstalled([]byte(`{"name":"dev"}`)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := prod.keep(ctx, []render.Credential{{Name: "prod-pg", Leaf: "password", Value: "s3cret"}}); err != nil {
		t.Fatal(err)
	}
	rec, exists, err := prod.ReadInstalled()
	if err != nil || !exists || string(rec) != `{"name":"prod","state":"installed"}` {
		t.Errorf("prod: %s %v %v", rec, exists, err)
	}
	if v, err := prod.resolver(ctx, nil)("prod-pg/password"); err != nil || v != "s3cret" {
		t.Errorf("kept credential: %q %v", v, err)
	}
	if _, ok, _ := dev.state.secret(ctx, "prod-pg", "password"); ok {
		t.Error("dev sees prod's credential")
	}
	if err := prod.WriteInstalled([]byte("not json")); err == nil {
		t.Error("a record that is not JSON is refused")
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode: %v %v", fi, err)
	}
}
