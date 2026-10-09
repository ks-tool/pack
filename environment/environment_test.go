package environment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// vault serves KV v2 at /v1/secret/data/<name> to the token "t" only.
func vault(t *testing.T, data map[string]map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "t" {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{"permission denied"}})
			return
		}
		kv, ok := data[strings.TrimPrefix(r.URL.Path, "/v1/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": kv, "metadata": map[string]any{"version": 1}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The file, Vault's KV over it, the command line over both; the token is
// never in the set.
func TestLoad(t *testing.T) {
	srv := vault(t, map[string]map[string]any{
		"secret/data/pack/common": {"namespace": "from-common", "registry": map[string]any{"user": "ci"}},
		"secret/data/pack/prod":   {"NAMESPACE": "prod", "kubeconfig": "apiVersion: v1"},
	})
	file := write(t, "prod.yaml", `
target: k8s
provider: oci://registry.example/pack-provider-k8s:1.2
vault:
  addr: `+srv.URL+`
  paths: [secret/data/pack/common, secret/data/pack/prod]
namespace: from-file
replicas: 3
`)
	t.Setenv(EnvVaultToken, "t")
	env, err := Load(context.Background(), file, map[string]string{"Replicas": "5"})
	if err != nil {
		t.Fatal(err)
	}
	if env.Target != "k8s" || env.Provider != "oci://registry.example/pack-provider-k8s:1.2" {
		t.Errorf("what pack reads: %+v", env)
	}
	if !reflect.DeepEqual(env.Vault.Paths, []string{"secret/data/pack/common", "secret/data/pack/prod"}) {
		t.Errorf("vault paths: %v", env.Vault.Paths)
	}
	want := map[string]string{
		"target": "k8s", "provider": "oci://registry.example/pack-provider-k8s:1.2",
		"vault_addr": srv.URL, "vault_paths": "secret/data/pack/common,secret/data/pack/prod",
		"namespace": "prod", "registry_user": "ci", "kubeconfig": "apiVersion: v1", "replicas": "5",
	}
	if !reflect.DeepEqual(env.Settings, want) {
		t.Errorf("the set:\n got %v\nwant %v", env.Settings, want)
	}
	for k, v := range env.Settings {
		if v == "t" || strings.Contains(k, "token") {
			t.Errorf("the Vault token leaked into the set: %s", k)
		}
	}
}

func TestLoad_Refusals(t *testing.T) {
	srv := vault(t, nil)
	file := write(t, "prod.yaml", "vault: {addr: "+srv.URL+", paths: [secret/data/x]}\n")
	t.Setenv(EnvVaultToken, "")
	if _, err := Load(context.Background(), file, nil); err == nil || !strings.Contains(err.Error(), EnvVaultToken) {
		t.Errorf("Vault paths without a token: %v", err)
	}
	t.Setenv(EnvVaultToken, "wrong")
	if _, err := Load(context.Background(), file, nil); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("a refused token: %v", err)
	}
	if _, err := Load(context.Background(), write(t, "prod.toml", "x = 1"), nil); err == nil {
		t.Error("an unknown format must be refused")
	}
	env, err := Load(context.Background(), "", map[string]string{"target": "fake"})
	if err != nil || env.Target != "fake" {
		t.Errorf("no file, the command line alone: %+v %v", env, err)
	}
}
