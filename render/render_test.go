package render

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/sdk/compute"
)

// testTarget is horchestra, spelled out here: render cannot import a
// target, and the properties below do not depend on which one it is.
type testTarget struct{ installation, namespace string }

func (n testTarget) Service(key string) string { return n.installation + "-" + key }
func (n testTarget) Instance(key string, i int) string {
	return fmt.Sprintf("%s-%d", n.Service(key), i)
}
func (n testTarget) Namespace() string { return n.namespace }
func (n testTarget) Address(key string, i int) string {
	if i < 0 {
		return n.Service(key) + "." + n.namespace
	}
	return fmt.Sprintf("%s.%s.%s", n.Instance(key, i), n.Service(key), n.namespace)
}
func (testTarget) PlatformSchema() []byte { return nil }
func (testTarget) Replicas(_ string, p map[string]any) (int, error) {
	if n, ok := p["replicas"].(float64); ok {
		return int(n), nil
	}
	return 1, nil
}
func (testTarget) Limits(_ string, p map[string]any) (map[string]any, error) {
	res, _ := p["resources"].(map[string]any)
	lim, _ := res["limits"].(map[string]any)
	out := map[string]any{}
	if m, ok := lim["memory"].(string); ok {
		q, err := compute.Quantity(m)
		if err != nil {
			return nil, err
		}
		out["memory"] = q / 1000
	}
	return out, nil
}
func (testTarget) ValidatePlatform(key string, p map[string]any, minimum map[string]string) []string {
	res, _ := p["resources"].(map[string]any)
	req, _ := res["requests"].(map[string]any)
	var out []string
	for name, min := range minimum {
		have, _ := req[name].(string)
		hv, _ := compute.Quantity(have)
		mv, _ := compute.Quantity(min)
		if hv < mv {
			out = append(out, fmt.Sprintf("/%s/platform/resources/requests/%s: %s is below the composition's minimum %s", key, name, have, min))
		}
	}
	return out
}

func ctxOf(installation, namespace string) Context {
	return Context{Installation: installation, Target: testTarget{installation, namespace}}
}

// The adh composition with its fixture values: the order, the peers and the
// formulas — what any target gets. Its golden output is horchestra's, in
// target/horchestra.
func TestRenderADH(t *testing.T) {
	root := filepath.Join("..", "examples")
	b, err := bundle.Load(filepath.Join(root, "adh"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "adh", "testdata", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var v Values
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	out, err := Render(b, v, ctxOf("prod", "data"))
	if err != nil {
		t.Fatal(err)
	}
	// the properties of the render worth stating
	order := out.Report.Order
	if len(order) != 4 || order[0] != "zookeeper" || order[1] != "kafka" {
		t.Errorf("order: %v", order)
	}
	if out.Report.Peers["kafka"]["zk"] != "zookeeper" {
		t.Errorf("kafka's zk role must auto-bind to the only zookeeper: %v", out.Report.Peers)
	}
	if out.Report.Computed["/kafka/jdk/heapMB"].(int64) != 24576 || out.Report.Computed["/zookeeper/jdk/heapMB"].(int64) != 768 {
		t.Errorf("heapMB per consumer: %v", out.Report.Computed)
	}
}

func TestRender_RefusesPlaintextPasswordAndAmbiguousPeer(t *testing.T) {
	root := filepath.Join("..", "examples")
	b, err := bundle.Load(filepath.Join(root, "adh"))
	if err != nil {
		t.Fatal(err)
	}
	v := Values{Values: map[string]any{"kafka": map[string]any{"jaas": map[string]any{"password": "plaintext"}}}}
	if _, err := Render(b, v, ctxOf("x", "n")); err == nil {
		t.Fatal("a plaintext password must be refused")
	}
}

// A values document naming only kafka installs kafka's REQUIRED peer,
// zookeeper, along with it — and leaves out ranger-db and hive-db, which
// nothing in that closure needs. Presence in values is the ask; the tool
// finds what a required role needs on its own.
func TestRender_SubsetPullsInRequiredPeersOnly(t *testing.T) {
	root := filepath.Join("..", "examples")
	b, err := bundle.Load(filepath.Join(root, "adh"))
	if err != nil {
		t.Fatal(err)
	}
	v := Values{Values: map[string]any{
		"kafka": map[string]any{
			"platform": map[string]any{"resources": map[string]any{"requests": map[string]any{"memory": "4Gi"}}},
			"jaas":     map[string]any{"password": map[string]any{"secretRef": map[string]any{"name": "kafka-sasl", "key": "password"}}},
		},
	}}
	out, err := Render(b, v, ctxOf("solo", "data"))
	if err != nil {
		t.Fatalf("render a kafka-only subset: %v", err)
	}

	if want := []string{"zookeeper", "kafka"}; !reflect.DeepEqual(out.Report.Order, want) {
		t.Errorf("order = %v, want %v — zookeeper (kafka's required zk peer) in, ranger-db/hive-db out", out.Report.Order, want)
	}
	if out.Report.Peers["kafka"]["zk"] != "zookeeper" {
		t.Errorf("kafka's zk role must still auto-bind to zookeeper in a subset render: %v", out.Report.Peers)
	}
	names := map[string]bool{}
	for _, u := range out.Units {
		names[u.Key] = true
	}
	if !names["zookeeper"] || !names["kafka"] {
		t.Errorf("the release is missing kafka or zookeeper: %v", names)
	}
	if names["ranger-db"] || names["hive-db"] {
		t.Errorf("a kafka-only install must not render postgres children nothing in its closure needs: %v", names)
	}
}

// --skip on a key nothing depends on just removes it — even one the values
// document itself named (skip overrides, it does not merely fail to add).
func TestRender_SkipExcludesAKeyNothingRequires(t *testing.T) {
	root := filepath.Join("..", "examples")
	b, err := bundle.Load(filepath.Join(root, "adh"))
	if err != nil {
		t.Fatal(err)
	}
	v := Values{
		Values: map[string]any{
			"kafka": map[string]any{
				"platform": map[string]any{"resources": map[string]any{"requests": map[string]any{"memory": "4Gi"}}},
				"jaas":     map[string]any{"password": map[string]any{"secretRef": map[string]any{"name": "kafka-sasl", "key": "password"}}},
			},
			"ranger-db": map[string]any{}, // named, then skipped — its own (incomplete) values must never be validated
		},
		Skip: []string{"ranger-db"},
	}
	out, err := Render(b, v, ctxOf("solo", "data"))
	if err != nil {
		t.Fatalf("skip a dependency-free key: %v", err)
	}
	if want := []string{"zookeeper", "kafka"}; !reflect.DeepEqual(out.Report.Order, want) {
		t.Errorf("order = %v, want %v — ranger-db named then skipped", out.Report.Order, want)
	}
}

// --skip on a key another installed key requires is refused, naming the
// dependent and the role.
func TestRender_SkipRefusesADependency(t *testing.T) {
	root := filepath.Join("..", "examples")
	b, err := bundle.Load(filepath.Join(root, "adh"))
	if err != nil {
		t.Fatal(err)
	}
	v := Values{
		Values: map[string]any{
			"kafka": map[string]any{
				"platform": map[string]any{"resources": map[string]any{"requests": map[string]any{"memory": "4Gi"}}},
				"jaas":     map[string]any{"password": map[string]any{"secretRef": map[string]any{"name": "kafka-sasl", "key": "password"}}},
			},
		},
		Skip: []string{"zookeeper"},
	}
	_, err = Render(b, v, ctxOf("solo", "data"))
	if err == nil {
		t.Fatal("kafka still requires zookeeper: skipping it must be refused")
	}
	if !strings.Contains(err.Error(), "kafka requires zookeeper") {
		t.Errorf("error %q does not name kafka as the dependent", err)
	}
}

// Skipping a key AND its only dependent together is not a contradiction —
// the check is against the post-skip closure, not the pre-skip one.
func TestRender_SkipBothADependencyAndItsOnlyDependent(t *testing.T) {
	root := filepath.Join("..", "examples")
	b, err := bundle.Load(filepath.Join(root, "adh"))
	if err != nil {
		t.Fatal(err)
	}
	v := Values{
		Values: map[string]any{
			"kafka": map[string]any{
				"platform": map[string]any{"resources": map[string]any{"requests": map[string]any{"memory": "4Gi"}}},
				"jaas":     map[string]any{"password": map[string]any{"secretRef": map[string]any{"name": "kafka-sasl", "key": "password"}}},
			},
		},
		Skip: []string{"kafka", "zookeeper"},
	}
	out, err := Render(b, v, ctxOf("solo", "data"))
	if err != nil {
		t.Fatalf("skip a key together with its only dependent: %v", err)
	}
	if len(out.Report.Order) != 0 {
		t.Errorf("order = %v, want none: both kafka and its zk peer were skipped", out.Report.Order)
	}
}

// Naming NONE of the composition's keys — a values document that only sets
// "common", which is not a component — keeps every key, the way a values
// document always has: an operator who has not thought about subsetting
// sees no change. Proven negatively: kafka's jaas.password has no default,
// so if the fallback had left kafka OUT of the selection (the bug this test
// exists to catch), this would render clean instead of failing on it.
func TestRender_NoSelectionInstallsEverything(t *testing.T) {
	root := filepath.Join("..", "examples")
	b, err := bundle.Load(filepath.Join(root, "adh"))
	if err != nil {
		t.Fatal(err)
	}
	// "common" is not a composition key, so this values document names ZERO
	// keys — the fallback, not the closure.
	v := Values{Values: map[string]any{"common": map[string]any{"realm": "CORP.EXAMPLE"}}}
	_, err = Render(b, v, ctxOf("full", "data"))
	if err == nil {
		t.Fatal("kafka's jaas.password has no default: an all-keys render must still fail validation")
	}
	for _, want := range []string{
		"/kafka/jaas: missing property 'password'",
		"/kafka/platform/resources/requests/memory: 2Gi is below the composition's minimum 4Gi",
		"/ranger-db/superuser: missing property 'password'",
		"/hive-db/superuser: missing property 'password'",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q: the fallback should have selected every key, not just kafka", err, want)
		}
	}
}
