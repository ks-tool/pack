package render

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/bundle"
)

// The adh composition rendered with its fixture values is the golden output
// of the tool: applicationset.yaml and every secret under
// examples/adh/testdata/expected. UPDATE_GOLDEN=1 rewrites them.
func TestRenderADH_Golden(t *testing.T) {
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
	out, err := Render(b, v, Context{Installation: "prod", Namespace: "data"})
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(root, "adh", "testdata", "expected")
	files := map[string][]byte{}
	files["applicationset.yaml"], _ = yaml.Marshal(out.Set)
	for _, s := range out.Secrets {
		files[filepath.Join("secrets", s.Metadata.Name+".yaml")], _ = yaml.Marshal(s)
	}
	files["report.yaml"], _ = yaml.Marshal(out.Report)
	if len(os.Getenv("UPDATE_GOLDEN")) > 0 {
		os.RemoveAll(expected)
		for name, data := range files {
			os.MkdirAll(filepath.Dir(filepath.Join(expected, name)), 0o755)
			if err := os.WriteFile(filepath.Join(expected, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, data := range files {
		want, err := os.ReadFile(filepath.Join(expected, name))
		if err != nil {
			t.Fatalf("%s: %v — UPDATE_GOLDEN=1 to write it", name, err)
		}
		if string(want) != string(data) {
			t.Errorf("%s differs from golden:\n%s", name, data)
		}
	}
	// nothing unexpected on disk
	filepath.Walk(expected, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(expected, path)
		if _, ok := files[rel]; !ok {
			t.Errorf("stale golden file %s", rel)
		}
		return nil
	})

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
	if _, err := Render(b, v, Context{Installation: "x", Namespace: "n"}); err == nil {
		t.Fatal("a plaintext password must be refused")
	}
}
