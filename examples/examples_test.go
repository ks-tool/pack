package examples

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ks-tool/pack/sdk/check"
	"github.com/ks-tool/pack/sdk/schema"
)

// Every committed schema.json is what its Go source generates — the same
// discipline as api/scheme/comments_gen.go: `go generate ./examples/...`.
func TestGeneratedSchemasAreCurrent(t *testing.T) {
	for name, v := range All() {
		want, err := Schema(v)
		if err != nil {
			t.Fatal(name, err)
		}
		got, err := os.ReadFile(filepath.Join(name, "schema.json"))
		if err != nil {
			t.Fatalf("%s: %v — run `go generate ./examples/...`", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s/schema.json is stale — run `go generate ./examples/...`", name)
		}
	}
}

// Every generated component passes the author checks of spec §2.6 with its
// libraries attached, the way `pack check` runs them.
func TestGeneratedSchemasPassCheck(t *testing.T) {
	for name := range All() {
		if name == "adh" {
			continue // a composition; its checks are §3.4, in the renderer
		}
		raw, err := Schema(All()[name])
		if err != nil {
			t.Fatal(err)
		}
		s, err := schema.Parse(raw)
		if err != nil {
			t.Fatal(name, err)
		}
		libRaw, _ := Schema(All()["jdk"])
		lib, _ := schema.Parse(libRaw)
		lib.Foreign = s.Foreign
		s.Attach("pack://jdk", lib)
		if err := s.AttachBuiltins(); err != nil {
			t.Fatal(err)
		}
		templates := map[string][]byte{}
		entries, _ := os.ReadDir(filepath.Join(name, "templates"))
		for _, e := range entries {
			b, _ := os.ReadFile(filepath.Join(name, "templates", e.Name()))
			templates[e.Name()] = b
		}
		ps, err := check.Run(check.Component{Name: name, Schema: s, Templates: templates, IsLibrary: name == "jdk"})
		if err != nil {
			t.Fatal(name, err)
		}
		for _, p := range ps {
			t.Errorf("%s: %s", name, p)
		}
	}
}
