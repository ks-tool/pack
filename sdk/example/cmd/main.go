// The author's generator: `go run ./cmd generate -o .` writes postgres/…,
// `go run ./cmd check` runs the author checks.
package main

import (
	"fmt"
	"os"

	"github.com/ks-tool/pack/sdk"
	"github.com/ks-tool/pack/sdk/example/postgres"
)

func main() {
	if err := sdk.Run(postgres.Defaults()); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
