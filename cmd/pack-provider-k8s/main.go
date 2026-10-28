// pack-provider-k8s is Kubernetes as a target, a program of its own: pack
// starts it, and it answers over the provider protocol what a key's platform
// means on the cluster, what a release becomes there — StatefulSets,
// Services, Secrets — and performs each step of a saga against the API
// server with client-go.
package main

import (
	"fmt"
	"os"

	"github.com/ks-tool/pack/provider"
	"github.com/ks-tool/pack/target/k8s"
)

func main() {
	if err := provider.Serve(k8s.Target{}); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pack-provider-k8s:", err)
		os.Exit(1)
	}
}
