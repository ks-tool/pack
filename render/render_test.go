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
)

// testTarget is horchestra, spelled out here: render cannot import a
// target, and the properties below do not depend on which one it is.
type testTarget struct{ installation, namespace string }

func (n testTarget) Namespace() string    { return n.namespace }
func (testTarget) PlatformSchema() []byte { return nil }
func (n testTarget) Resolve(key, scale string, p map[string]any, _ bool) (*Resolved, error) {
	svc := n.installation + "-" + key
	res := &Resolved{Service: svc, Address: svc + "." + n.namespace, Platform: map[string]any{}}
	if scale == "replicas" {
		for i := range 3 {
			name := fmt.Sprintf("%s-%d", svc, i)
			res.Instances = append(res.Instances, Instance{Name: name, Address: name + "." + svc + "." + n.namespace})
		}
	}
	return res, nil
}
func (testTarget) ValidatePlatform(string, map[string]any) []string { return nil }

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

// pack.keys names every key of the installation — a single key by its one
// address, a scaled one by each instance — without binding any of them: the
// key that reads it is ordered by its own peers only.
func TestKeysContext(t *testing.T) {
	r := &render{
		keys: []string{"ca", "zookeeper"},
		doc:  map[string]any{"zookeeper": map[string]any{"ports": map[string]any{"client": 2181}}},
		resolved: map[string]*Resolved{
			"ca": {Service: "prod-ca", Address: "prod-ca"},
			"zookeeper": {Service: "prod-zookeeper", Instances: []Instance{
				{Name: "prod-zookeeper-0", Address: "prod-zookeeper-0"}, {Name: "prod-zookeeper-1", Address: "prod-zookeeper-1"}}},
		},
		ctx: ctxOf("prod", "data"),
	}
	keys := r.packContext("ca", nil)["keys"].(map[string]any)
	ca, zk := keys["ca"].(map[string]any), keys["zookeeper"].(map[string]any)
	if !reflect.DeepEqual(ca["instances"], []any{"prod-ca"}) || ca["scaled"] != false {
		t.Errorf("ca: %v", ca)
	}
	if !reflect.DeepEqual(zk["instances"], []any{"prod-zookeeper-0", "prod-zookeeper-1"}) || zk["scaled"] != true ||
		zk["ports"].(map[string]any)["client"] != 2181 {
		t.Errorf("zookeeper: %v", zk)
	}
}

// neighbours writes a composition of two keys: src, which has a computed leaf
// (realm), a default list (principals) and a password, and dst, which reads
// them through its peer role src. dst sorts before src, so a formula of dst
// that did not wait for src's own formula would see nothing.
func neighbours(t *testing.T, dstProps string) *bundle.Bundle {
	t.Helper()
	dir := t.TempDir()
	write := func(name, doc string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "schema.json"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src", `{
	  "$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false,
	  "x-pack": {"scale": "single"},
	  "properties": {
	    "platform": {"$ref": "urn:pack:platform", "default": {}},
	    "principals": {"type": "array", "items": {"type": "string"}, "default": ["hdfs", "HTTP"]},
	    "realm": {"type": "string", "x-pack-compute": "common.realm + '.LOCAL'"},
	    "conf": {"type": "object", "properties": {"mode": {"type": "string", "default": "strict"}}},
	    "secret": {"type": "string", "format": "password"}
	  }
	}`)
	write("dst", `{
	  "$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false,
	  "x-pack": {"scale": "single", "peers": {"src": {"schema": "src", "required": true}}},
	  "properties": {
	    "platform": {"$ref": "urn:pack:platform", "default": {}},
	    `+dstProps+`
	  }
	}`)
	for _, name := range []string{"src", "dst"} {
		pf := "type: application/vnd.horchestra.component.v1+json\nannotations:\n  io.horchestra.name: " + name + "\n  io.horchestra.version: \"1.0.0\"\nitems:\n  - from: oci://images/" + name + ":1.0.0\n"
		if err := os.WriteFile(filepath.Join(dir, name, "pack.yaml"), []byte(pf), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("comp", `{
	  "$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false,
	  "properties": {
	    "common": {"type": "object", "properties": {"realm": {"type": "string", "default": "EX"}}},
	    "dst": {"$ref": "pack://dst#"},
	    "src": {"$ref": "pack://src#"}
	  }
	}`)
	b, err := bundle.Load(filepath.Join(dir, "comp"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A formula reads its neighbour's values: what the neighbour's defaults say,
// and what the neighbour's own formula computes — it waits for that formula.
func TestRender_ReadsNeighbourValues(t *testing.T) {
	b := neighbours(t, `
	    "principals": {"type": "array", "items": {"type": "string"}, "x-pack-compute": "peers.src.values.principals"},
	    "realm": {"type": "string", "x-pack-compute": "peers.src.values.realm"},
	    "mode": {"type": "string", "x-pack-compute": "peers.src.values.conf.mode"},
	    "all": {"type": "object", "x-pack-compute": "peers.src.values.conf"}`)
	out, err := Render(b, Values{Values: map[string]any{"src": map[string]any{}, "dst": map[string]any{}}}, ctxOf("x", "n"))
	if err != nil {
		t.Fatal(err)
	}
	c := out.Report.Computed
	if got := fmt.Sprint(c["/dst/principals"]); got != "[hdfs HTTP]" {
		t.Errorf("a default of the neighbour: %v", c["/dst/principals"])
	}
	if c["/dst/realm"] != "EX.LOCAL" {
		t.Errorf("dst sorts before src: its formula waits for src's, got %v", c["/dst/realm"])
	}
	if c["/dst/mode"] != "strict" {
		t.Errorf("a nested value: %v", c["/dst/mode"])
	}
	if m, _ := c["/dst/all"].(map[string]any); m["mode"] != "strict" {
		t.Errorf("a subtree: %v", c["/dst/all"])
	}
}

// The operator's value for the neighbour is what the formula sees; a password
// leaf is not shown at all, even as a reference.
func TestRender_NeighbourValuesAreTheOperatorsAndHideCredentials(t *testing.T) {
	b := neighbours(t, `
	    "principals": {"type": "array", "items": {"type": "string"}, "x-pack-compute": "peers.src.values.principals"},
	    "seen": {"type": "string", "x-pack-compute": "has(peers.src.values.secret) ? 'visible' : 'hidden'"},
	    "keys": {"type": "array", "items": {"type": "string"}, "x-pack-compute": "peers.src.values.map(k, k)"}`)
	v := Values{Values: map[string]any{
		"src": map[string]any{
			"principals": []any{"yarn"},
			"secret":     map[string]any{"secretRef": map[string]any{"name": "s", "key": "k"}},
		},
		"dst": map[string]any{},
	}}
	out, err := Render(b, v, ctxOf("x", "n"))
	if err != nil {
		t.Fatal(err)
	}
	c := out.Report.Computed
	if got := fmt.Sprint(c["/dst/principals"]); got != "[yarn]" {
		t.Errorf("the operator's list: %v", c["/dst/principals"])
	}
	if c["/dst/seen"] != "hidden" {
		t.Errorf("a password leaf of the neighbour must not be visible: %v", c["/dst/seen"])
	}
	for _, k := range c["/dst/keys"].([]any) {
		if k == "secret" {
			t.Errorf("the neighbour's keys include its password: %v", c["/dst/keys"])
		}
	}
}

// pack check says so when a formula reaches a neighbour's password.
func TestCheckBundle_NeighbourPassword(t *testing.T) {
	b := neighbours(t, `"leak": {"type": "string", "x-pack-compute": "string(peers.src.values.secret)"}`)
	ps, err := CheckBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range ps {
		found = found || strings.Contains(p.Message, "password /src/secret of its neighbour")
	}
	if !found {
		t.Errorf("a read of the neighbour's password must be reported: %v", ps)
	}
}
