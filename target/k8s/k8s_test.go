package k8s

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

// adh renders the adh example into its Kubernetes environment.
func adh(t *testing.T) (*Installation, *render.Release) {
	t.Helper()
	root := filepath.Join("..", "..", "examples", "adh")
	env, err := environment.Load(context.Background(), filepath.Join(root, "envs", "k8s.yaml"), nil)
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

// The adh composition in its Kubernetes environment is this target's golden
// output: a stream per key under testdata/adh/expected. UPDATE_GOLDEN=1
// rewrites them.
func TestConvertADH_Golden(t *testing.T) {
	inst, rel := adh(t)
	out, err := inst.Convert(rel, nil)
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join("testdata", "adh", "expected")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		_ = os.RemoveAll(expected)
		_ = os.MkdirAll(expected, 0o755)
		for _, f := range out.Files {
			if err := os.WriteFile(filepath.Join(expected, f.Path), f.Data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, f := range out.Files {
		want, err := os.ReadFile(filepath.Join(expected, f.Path))
		if err != nil {
			t.Fatalf("%s: %v — UPDATE_GOLDEN=1 to write it", f.Path, err)
		}
		if string(want) != string(f.Data) {
			t.Errorf("%s differs from golden:\n%s", f.Path, f.Data)
		}
	}
}

// What the cluster runs, stated: names and addresses of a StatefulSet's pods,
// a per-instance file as the pod's own key by subPathExpr, a library as an
// image volume, the environment's replicas and nodeSelector.
func TestConvertADH_Properties(t *testing.T) {
	inst, rel := adh(t)
	out, err := inst.Convert(rel, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range out.Files {
		files[f.Path] = string(f.Data)
	}
	zk, kafka := files["zookeeper.yaml"], files["kafka.yaml"]
	for _, want := range []string{"kind: StatefulSet", "replicas: 3", "subPathExpr: $(POD_NAME)/myid", "path: prod-zookeeper-2/myid", "fieldPath: metadata.name", "clusterIP: None"} {
		if !strings.Contains(zk, want) {
			t.Errorf("zookeeper lacks %q", want)
		}
	}
	for _, want := range []string{"reference: images/jdk:21.0.4", "node-role.kubernetes.io/data: \"true\"", "prod-zookeeper-0.prod-zookeeper.data.svc:2181", "podAntiAffinity"} {
		if !strings.Contains(kafka, want) {
			t.Errorf("kafka lacks %q", want)
		}
	}
	if !strings.Contains(kafka, "${secret:prod-kafka-sasl/password}") {
		t.Error("a placeholder stays in what is rendered for reading")
	}
}

func TestSubstitute(t *testing.T) {
	got, err := substitute("user=a\npassword=${secret:pg/password}\n", func(ref string) (string, error) {
		if ref != "pg/password" {
			t.Errorf("ref %q", ref)
		}
		return "s3cret", nil
	})
	if err != nil || got != "user=a\npassword=s3cret\n" {
		t.Errorf("%q %v", got, err)
	}
}
