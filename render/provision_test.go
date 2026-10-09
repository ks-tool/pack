package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ks-tool/pack/bundle"
)

func TestProvision(t *testing.T) {
	b, err := bundle.Load(filepath.Join("..", "examples", "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := ctxOf("dev", "apps")

	// generate: a Secret per pointer, the reference written into the values, the render sees it
	v := Values{Values: map[string]any{"postgres": map[string]any{"database": "app"}}}
	creds, err := Provision(b, &v, ctx.Installation, "generate", []string{"/postgres/superuser/password"})
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].Name != "dev-postgres-superuser-password" || creds[0].Leaf != "password" || creds[0].Key != "postgres" || len(creds[0].Value) < 20 {
		t.Fatalf("credential: %+v", creds)
	}
	out, err := Render(b, v, ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range out.Units[0].Env {
		if e.Name == "POSTGRES_PASSWORD" && e.Secret != nil && e.Secret.Name == "dev-postgres-superuser-password" {
			found = true
		}
	}
	if !found {
		t.Errorf("the provisioned reference must reach env: %+v", out.Units[0].Env)
	}

	// file provider
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "postgres.superuser.password"), []byte("from-file\n"), 0o600)
	v = Values{Values: map[string]any{"postgres": map[string]any{"database": "app"}}}
	creds, err = Provision(b, &v, ctx.Installation, Provider("file:"+dir), []string{"postgres/superuser/password"})
	if err != nil || creds[0].Value != "from-file" {
		t.Fatalf("file provider: %v %+v", err, creds)
	}

	// env provider
	t.Setenv("PACK_SECRET_POSTGRES_SUPERUSER_PASSWORD", "from-env")
	v = Values{Values: map[string]any{"postgres": map[string]any{"database": "app"}}}
	creds, err = Provision(b, &v, ctx.Installation, "env", []string{"/postgres/superuser/password"})
	if err != nil || creds[0].Value != "from-env" {
		t.Fatalf("env provider: %v %+v", err, creds)
	}

	// refusals: not a password; already referenced; unknown provider; --secret without a provider
	v = Values{}
	if _, err := Provision(b, &v, ctx.Installation, "generate", []string{"/postgres/database"}); err == nil || !strings.Contains(err.Error(), "not a password") {
		t.Errorf("a non-password leaf must be refused: %v", err)
	}
	v = Values{Values: map[string]any{"postgres": map[string]any{"superuser": map[string]any{"password": map[string]any{"secretRef": map[string]any{"name": "x", "key": "k"}}}}}}
	if _, err := Provision(b, &v, ctx.Installation, "generate", []string{"/postgres/superuser/password"}); err == nil {
		t.Error("a leaf the operator already referenced must be refused")
	}
	if _, err := Provision(b, &v, ctx.Installation, "vault", nil); err == nil {
		t.Error("an unknown provider must be refused")
	}
	if _, err := Provision(b, &v, ctx.Installation, "", []string{"/x"}); err == nil {
		t.Error("--secret without a provider must be refused")
	}
}

// A library writing a file the consumer also writes is refused by the
// composition check — and, should it slip through, by the converter.
func TestCheckBundle_FileOwnedTwice(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join("..", "examples")
	for _, name := range []string{"kafka", "jdk"} {
		copyTree(t, filepath.Join(src, name), filepath.Join(root, name))
	}
	// give the jdk library a file at kafka's server.properties path
	sp := filepath.Join(root, "jdk", "schema.json")
	raw, _ := os.ReadFile(sp)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	heapOpts, _ := doc["properties"].(map[string]any)["heapOpts"].(map[string]any)
	if heapOpts == nil {
		t.Fatal("fixture: heapOpts not found")
	}
	heapOpts["x-pack-file"] = map[string]any{"path": "/opt/kafka/config/server.properties", "format": "text"}
	patched, _ := json.Marshal(doc)
	os.WriteFile(sp, patched, 0o644)
	b, err := bundle.Load(filepath.Join(root, "kafka"))
	if err != nil {
		t.Fatal(err)
	}
	ps, err := CheckBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range ps {
		if strings.Contains(p.Message, "also written by") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the path conflict to be reported: %v", ps)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, _ := os.ReadFile(path)
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
}
