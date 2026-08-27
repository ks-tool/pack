// gen writes schema.json for every example — `go generate ./examples/...`
// runs it, and the test beside it fails when a file drifts from its Go source.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ks-tool/pack/examples"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	for name, v := range examples.All() {
		s, err := examples.Schema(v)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, name+":", err)
			os.Exit(1)
		}
		if err = os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err = os.WriteFile(filepath.Join(root, name, "schema.json"), s, 0o644); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("wrote", filepath.Join(root, name, "schema.json"))
	}
	_ = json.Marshal
}
