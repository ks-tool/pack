// Package target is what the core asks of a system a pack is installed on.
// A pack knows no target: the core renders to render.Release, and a target —
// horchestra, Kubernetes, VMs — names what runs, reaches it, and turns the
// release into its own objects. This is the in-process form of the provider
// protocol; a provider in its own process speaks the same over gRPC.
package target

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/ks-tool/pack/render"
)

// Target is a system a pack can be installed on.
type Target interface {
	// Name is how an operator selects it (--target).
	Name() string
	// Configure binds the target to one installation: the name the release
	// is installed under, and the target's own settings (a namespace, an
	// inventory). A setting the target does not know is refused.
	Configure(installation string, settings Settings) (Installation, error)
}

// Installation is a target bound to one installation: what the render asks
// of it — names, the platform's schema and meaning — and the conversion.
type Installation interface {
	render.Target
	// Convert turns a release, and the credentials provisioned for it, into
	// the target's objects.
	Convert(rel *render.Release, credentials []render.Credential) (*Output, error)
}

// Settings are a target's own words, key=value.
type Settings map[string]string

// Take removes and returns a setting, or def when it is absent.
func (s Settings) Take(key, def string) string {
	v, ok := s[key]
	if !ok {
		return def
	}
	delete(s, key)
	return v
}

// Rest refuses what nobody took — a typo must not pass as a default.
func (s Settings) Rest(target string) error {
	if len(s) == 0 {
		return nil
	}
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Errorf("target %s: unknown setting %s", target, strings.Join(keys, ", "))
}

// Output is what a target makes of a release, as files: the CLI writes them
// under -o, or streams those marked Stream to stdout.
type Output struct {
	Files  []File
	Report render.Report
}

// File is one object of the target, serialised.
type File struct {
	Path string
	Data []byte
	Mode fs.FileMode
	// Stream: part of what `pack template` prints without -o — the objects
	// to apply, not the operator's notes about them.
	Stream bool
}
