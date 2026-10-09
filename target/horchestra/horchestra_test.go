package horchestra

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/render"
)

// The adh composition rendered with its fixture values is the golden output
// of the tool: applicationset.yaml and every secret under
// examples/adh/testdata/expected. UPDATE_GOLDEN=1 rewrites them.
func TestRenderADH_Golden(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	b, err := bundle.Load(filepath.Join(root, "adh"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "adh", "testdata", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var v render.Values
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
}
