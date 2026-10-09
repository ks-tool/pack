package example

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ks-tool/pack/sdk"
	"github.com/ks-tool/pack/sdk/example/postgres"
)

// The SDK version of examples/postgres yields the very same schema.json:
// embedding sdk.Workload is spelling the same tags.
func TestSDKPostgresMatchesExample(t *testing.T) {
	got, err := sdk.Schema(postgres.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("..", "..", "examples", "postgres", "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("schema differs from examples/postgres/schema.json:\n%s", got)
	}
	ps, err := sdk.Verify(postgres.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		t.Errorf("check: %s", p)
	}
	pf, err := sdk.PackFile(postgres.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if pf.Type != sdk.ArtifactComponent || len(pf.Items) != 3 || pf.Items[0].From != "oci://localhost:5001/images/postgres:16.3" ||
		pf.Items[0].Platform != "linux/amd64" || pf.Items[2].From != "dir://templates/" || pf.Items[2].Type != sdk.ArtifactTemplate {
		t.Errorf("pack file: %+v", pf)
	}
	raw, err := sdk.PackFileYAML(postgres.Defaults())
	if err != nil || !bytes.Contains(raw, []byte("io.horchestra.name: postgres")) {
		t.Errorf("pack.yaml:\n%s %v", raw, err)
	}
	dir := t.TempDir()
	run := func(args ...string) error { // Run reads os.Args, as an author's main would
		os.Args = append([]string{"gen"}, args...)
		return sdk.Run(postgres.Defaults())
	}
	if err := run("generate", "-o", dir); err != nil {
		t.Fatal(err)
	}
	if err := run("check"); err != nil {
		t.Errorf("check: %v", err)
	}
	if err := run("schema"); err == nil {
		t.Error("schema without a name must fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "postgres", "templates", "init-extensions.sh")); err != nil {
		t.Errorf("template not written: %v", err)
	}
}
