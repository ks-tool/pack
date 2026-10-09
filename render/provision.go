package render

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/sdk/schema"
)

// Provider is where the values of the credentials the tool itself creates
// come from — the case without a Vault, where nobody made the Secret ahead of
// time. The tool never reads a Vault; Vault-backed secrets are pointers the
// operator creates beforehand and references from the values.
//
//	generate      a random value per leaf
//	env           $PACK_SECRET_<POINTER>, e.g. PACK_SECRET_KAFKA_JAAS_PASSWORD
//	file:<dir>    <dir>/<pointer with dots>, e.g. <dir>/kafka.jaas.password
type Provider string

// Credential is one value the tool provisioned: the secret a target creates
// for it, and the key of the leaf. Key is the system key it belongs to.
type Credential struct {
	Name  string
	Leaf  string
	Value string
	Key   string
}

// Provision reads a credential for every listed pointer — an explicit
// allowlist: only these leaves are read from the provider, anything else is
// untouched — and writes the reference into the values, so that the render
// sees a reference like any other; the target creates the secrets. Each
// pointer must be a password leaf of the composition's schema the operator
// did not already fill.
func Provision(b *bundle.Bundle, v *Values, installation string, provider Provider, pointers []string) ([]Credential, error) {
	if len(provider) == 0 {
		if len(pointers) > 0 {
			return nil, fmt.Errorf("--secret needs --secretProvider")
		}
		return nil, nil
	}
	kind, arg, _ := strings.Cut(string(provider), ":")
	switch kind {
	case "generate", "env", "file":
	default:
		return nil, fmt.Errorf("--secretProvider %q: generate, env or file:<dir>", provider)
	}
	if kind == "file" && len(arg) == 0 {
		return nil, fmt.Errorf("--secretProvider file: needs a directory (file:<dir>)")
	}
	if v.Values == nil {
		v.Values = map[string]any{}
	}
	var out []Credential
	sort.Strings(pointers)
	for _, ptr := range pointers {
		if !strings.HasPrefix(ptr, "/") {
			ptr = "/" + ptr
		}
		_, node, err := b.Composition.Lookup(ptr)
		if err != nil {
			return nil, fmt.Errorf("--secret %s: %w", ptr, err)
		}
		if !node.IsPassword() {
			return nil, fmt.Errorf("--secret %s: not a password leaf (format: password)", ptr)
		}
		if cur, set := get(v.Values, ptr); set && cur != nil {
			return nil, fmt.Errorf("--secret %s: the values already hold a reference for it", ptr)
		}
		segs := schema.Split(ptr)
		key := segs[0]
		name := installation + "-" + strings.Join(segs, "-")
		leaf := segs[len(segs)-1]
		value, err := fetch(kind, arg, ptr)
		if err != nil {
			return nil, err
		}
		out = append(out, Credential{Name: name, Leaf: leaf, Value: value, Key: key})
		if err := set(v.Values, ptr, map[string]any{"secretRef": map[string]any{"name": name, "key": leaf}}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func fetch(kind, arg, ptr string) (string, error) {
	dotted := strings.ReplaceAll(strings.TrimPrefix(ptr, "/"), "/", ".")
	switch kind {
	case "generate":
		var buf [24]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(buf[:]), nil
	case "env":
		name := "PACK_SECRET_" + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(dotted))
		val, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("--secret %s: $%s is not set", ptr, name)
		}
		return val, nil
	case "file":
		raw, err := os.ReadFile(filepath.Join(arg, dotted))
		if err != nil {
			return "", fmt.Errorf("--secret %s: %w", ptr, err)
		}
		return strings.TrimRight(string(raw), "\n"), nil
	}
	return "", fmt.Errorf("provider %q", kind)
}
