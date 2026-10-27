package horchestra

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

// adhEnvironment is the horchestra environment of the adh example: its set.
func adhEnvironment(t *testing.T) target.Settings {
	t.Helper()
	env, err := environment.Load(context.Background(), filepath.Join("..", "..", "examples", "adh", "envs", "horchestra.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return env.Settings
}

// The adh composition rendered with its fixture values into its horchestra
// environment is horchestra's golden output: applicationset.yaml and every
// secret under testdata/adh/expected. UPDATE_GOLDEN=1 rewrites them.
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
	out, err := Render(b, v, Context{Installation: "prod", Namespace: "data", Settings: adhEnvironment(t)})
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join("testdata", "adh", "expected")
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

// As a target.Target: the namespace setting, a setting of another target
// refused, and the files the CLI writes — the same bytes as the golden set.
func TestTarget(t *testing.T) {
	settings := adhEnvironment(t)
	settings["kubeconfig"] = "x"
	if _, err := (Target{}).Configure("prod", settings); err == nil || !strings.Contains(err.Error(), "no setting kubeconfig") {
		t.Errorf("a setting horchestra has not is refused: %v", err)
	}
	delete(settings, "kubeconfig")
	settings["provider"], settings["vault_addr"] = "pack-provider-horchestra", "https://vault.example" // pack's own
	inst, err := Target{}.Configure("prod", settings)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..", "examples")
	b, err := bundle.Load(filepath.Join(root, "adh"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "adh", "testdata", "values.yaml"))
	var v render.Values
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	rel, err := render.Render(b, v, render.Context{Installation: "prod", Target: inst})
	if err != nil {
		t.Fatal(err)
	}
	creds := []render.Credential{{Name: "prod-kafka-jaas-password", Leaf: "password", Value: "x", Key: "kafka"}}
	out, err := inst.Convert(rel, creds)
	if err != nil {
		t.Fatal(err)
	}
	if out.Files[0].Path != "applicationset.yaml" || !out.Files[0].Stream {
		t.Fatalf("the set comes first and is streamed: %+v", out.Files[0].Path)
	}
	want, _ := os.ReadFile(filepath.Join("testdata", "adh", "expected", "applicationset.yaml"))
	if string(out.Files[0].Data) != string(want) {
		t.Error("applicationset.yaml differs from golden")
	}
	if out.Files[1].Path != "secrets/prod-kafka-jaas-password.yaml" || out.Files[1].Mode != 0o600 {
		t.Errorf("provisioned credentials come before config secrets, 0600: %s %v", out.Files[1].Path, out.Files[1].Mode)
	}
	last := out.Files[len(out.Files)-1]
	if last.Path != "report.yaml" || last.Stream {
		t.Errorf("the report is a file, not part of the stream: %+v", last.Path)
	}
}

// The platform subtree is validated by horchestra's schema, not the pack's:
// placement passes, volumes — gone from the schema — is refused.
func TestPlatformSchema(t *testing.T) {
	b, err := bundle.Load(filepath.Join("..", "..", "examples", "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	pg := func(platform map[string]any) render.Values {
		return render.Values{Values: map[string]any{"postgres": map[string]any{
			"database":  "app",
			"superuser": map[string]any{"password": map[string]any{"secretRef": map[string]any{"name": "pg", "key": "password"}}},
			"platform":  platform,
		}}}
	}
	ctx := Context{Installation: "dev", Namespace: "apps"}
	out, err := Render(b, pg(map[string]any{"placement": map[string]any{"queue": "q"}}), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pl := out.Set.Spec.Applications[0].Spec.Placement; pl == nil || pl.Queue != "q" {
		t.Errorf("placement is horchestra's to read: %+v", pl)
	}
	if _, err := Render(b, pg(map[string]any{"volumes": []any{map[string]any{}}}), ctx); err == nil || !strings.Contains(err.Error(), "'volumes' not allowed") {
		t.Errorf("platform.volumes must be refused: %v", err)
	}
}
