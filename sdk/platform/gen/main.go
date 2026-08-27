// gen writes pack_v1.json — the vocabulary schema — from the Go types in
// packschema. `go generate ./platform/...` runs it; the test beside
// it fails when the file drifts.
package main

import (
	"fmt"
	"os"

	"github.com/ks-tool/pack/sdk/packschema"
)

func main() {
	raw, err := packschema.Vocabulary()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err = os.WriteFile("pack_v1.json", raw, 0o644); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("wrote pack_v1.json")
}
