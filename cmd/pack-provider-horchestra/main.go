// pack-provider-horchestra is the horchestra target as a program of its own:
// pack starts it, and it answers over the provider protocol what a key's
// platform means on horchestra and what a release becomes there — an
// ApplicationSet and its config Secrets.
package main

import (
	"fmt"
	"os"

	"github.com/ks-tool/pack/provider"
	"github.com/ks-tool/pack/target/horchestra"
)

func main() {
	if err := provider.Serve(horchestra.Target{}); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pack-provider-horchestra:", err)
		os.Exit(1)
	}
}
