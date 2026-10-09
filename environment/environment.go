// Package environment is where a pack is installed, as pack is told it: a
// file per environment and the Vault KV that file names — the same command,
// the same file, the same result, on a console or in CI. A pack described
// once (its values) goes to as many environments as there are files.
//
// The file, YAML or JSON, flattened by the app-config rule ({vault: {addr:
// …}} is vault_addr):
//
//	target: k8s                                # the system; its provider is pack-provider-k8s …
//	provider: oci://registry.example/pack-provider-k8s:1.2   # … or this binary or image
//	vault:
//	  addr: https://vault.example:8200         # VAULT_ADDR when the file has none
//	  paths: [secret/data/pack/prod]           # KV read over the file, the last path winning
//	namespace: data                            # and anything else the target wants
//
// The Vault token comes from VAULT_TOKEN only — it is never in the set.
// Everything else — the file's keys, Vault's over them, the command line's
// (--set) over both — is the set the provider gets, whole.
package environment

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	appconfig "github.com/ks-tool/app-config/go"
	"sigs.k8s.io/yaml"
)

// EnvVaultToken is where the Vault token comes from.
const EnvVaultToken = "VAULT_TOKEN"

// Environment is one environment: what pack itself reads of the set, and
// the set.
type Environment struct {
	Target   string `env:"TARGET"`
	Provider string `env:"PROVIDER"`
	Vault    Vault  `envPrefix:"VAULT_"`
	// Settings is the whole set, keys in lower case: what the provider gets.
	Settings map[string]string `env:"-"`
}

type Vault struct {
	Addr  string   `env:"ADDR"`
	Paths []string `env:"PATHS"`
}

// Load reads the file (none — an empty one), then the Vault KV it names,
// then overrides, each over the one before.
func Load(ctx context.Context, file string, overrides map[string]string) (*Environment, error) {
	fileKV, err := readFile(file)
	if err != nil {
		return nil, err
	}
	over := lower(overrides)
	env := &Environment{}
	if err := appconfig.Load(env, appconfig.KV(fileKV), appconfig.KV(over)); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var vaultKV map[string]string
	if len(env.Vault.Paths) > 0 {
		addr := env.Vault.Addr
		if len(addr) == 0 {
			addr = os.Getenv("VAULT_ADDR")
		}
		if len(addr) == 0 {
			return nil, fmt.Errorf("%s: vault.paths without vault.addr (or VAULT_ADDR)", file)
		}
		token := os.Getenv(EnvVaultToken)
		if len(token) == 0 {
			return nil, fmt.Errorf("%s: vault.paths need %s", file, EnvVaultToken)
		}
		// path by path: the last path wins over the ones before it however a
		// key is spelled — one Read of all of them would keep namespace and
		// NAMESPACE apart, and lowering them after would pick one at random
		v := appconfig.NewVault(addr, appconfig.VaultToken{Token: token})
		vaultKV = map[string]string{}
		for _, path := range env.Vault.Paths {
			kv, err := v.Read(ctx, path)
			if err != nil {
				return nil, err
			}
			maps.Copy(vaultKV, lower(kv))
		}
	}
	set := map[string]string{}
	maps.Copy(set, fileKV)
	maps.Copy(set, vaultKV)
	maps.Copy(set, over)
	env = &Environment{}
	if err := appconfig.Load(env, appconfig.KV(set)); err != nil {
		return nil, err
	}
	env.Settings = set
	return env, nil
}

// readFile flattens the environment file into keys in lower case.
func readFile(name string) (map[string]string, error) {
	if len(name) == 0 {
		return map[string]string{}, nil
	}
	raw, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var dec appconfig.Decoder
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml":
		dec = appconfig.DecodeTree(func(b []byte, v any) error { return yaml.Unmarshal(b, v) })
	case ".json":
		dec = appconfig.DecodeTree(json.Unmarshal)
	default:
		return nil, fmt.Errorf("%s: an environment is a .yaml or a .json file", name)
	}
	kv, err := dec(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return lower(kv), nil
}

// lower: one spelling per key, so a file's namespace and Vault's NAMESPACE
// are one setting; app-config matches them case-blind anyway.
func lower(kv map[string]string) map[string]string {
	out := make(map[string]string, len(kv))
	for k, v := range kv {
		out[strings.ToLower(k)] = v
	}
	return out
}
