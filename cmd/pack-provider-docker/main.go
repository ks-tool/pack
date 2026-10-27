// pack-provider-docker is a Docker engine as a target, a program of its own:
// pack starts it, and it answers over the provider protocol what a key's
// platform means on the engine, what a release becomes there — a compose
// file — and performs each step of a saga with the docker CLI.
package main

import (
	"fmt"
	"os"

	"github.com/ks-tool/pack/provider"
	"github.com/ks-tool/pack/target/docker"
)

func main() {
	if err := provider.Serve(docker.Target{}); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pack-provider-docker:", err)
		os.Exit(1)
	}
}
