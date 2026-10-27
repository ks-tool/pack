// Package target is what the core asks of a system a pack is installed on.
// A pack knows no target: the core renders to render.Release, and a target —
// horchestra, Kubernetes, VMs — names what runs, reaches it, and turns the
// release into its own objects. This is the in-process form of the provider
// protocol; a provider in its own process speaks the same over gRPC.
package target

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/ks-tool/pack/render"
)

// Target is a system a pack can be installed on.
type Target interface {
	// Name is how an operator selects it (--target).
	Name() string
	// Configure binds the target to one installation: the name the release
	// is installed under, and the environment's whole set (package
	// environment: its file, its Vault KV, the command line) — the target
	// reads what is its own (a namespace, an inventory, credentials) and
	// leaves the rest.
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

// CapExecute: the target performs saga steps and keeps the installation's
// record — its installations are Executors.
const CapExecute = "execute"

// Capable is a target that says what it can do beyond rendering.
type Capable interface {
	Capabilities() []string
}

// Executor is an installation that performs the steps of a saga pack drives,
// and keeps pack's record of the installation in the target's own system.
type Executor interface {
	// ReadInstalled returns the record as pack last wrote it.
	ReadInstalled() (record []byte, exists bool, err error)
	// WriteInstalled keeps the record verbatim.
	WriteInstalled(record []byte) error
	// Execute performs one step, telling events what it does; nil — done.
	Execute(ctx context.Context, step Step, events func(string)) error
}

// Step is one step of a saga as a target performs it: the plan's step, and
// what it acts on — the key's unit in the version being installed and in the
// installed one, the credentials provisioned for the key.
type Step struct {
	N         int
	Key       string
	Kind      string // stop | hook | apply | wait | remove
	Name      string // a hook's name
	Phase     string
	Timeout   string
	Exclusive bool
	Image     string   // a hook's image
	Command   []string // a hook's command
	// Rollback: the step undoes an earlier one — an apply of Previous, a
	// remove of a key that was added, a hook's compensation.
	Rollback    bool
	Unit        *render.Unit
	Previous    *render.Unit
	Credentials []render.Credential
}

// Settings are the environment's set, key=value, keys in lower case.
type Settings map[string]string

// Own are the keys of the set that are pack's own, not a target's: which
// target, its provider, the Vault the set was read from.
var Own = []string{"target", "provider", "vault_addr", "vault_paths"}

// Check refuses a setting the target does not know: every key must be pack's
// own, one of names, or start with one of prefixes. A setting nothing reads
// is a mistake — a typo, a setting of another target — never something to
// pass over.
func (s Settings) Check(target string, names, prefixes []string) error {
	var unknown []string
	for k := range s {
		if slices.Contains(Own, k) || slices.Contains(names, k) ||
			slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(k, p) }) {
			continue
		}
		unknown = append(unknown, k)
	}
	if len(unknown) == 0 {
		return nil
	}
	slices.Sort(unknown)
	has := slices.Clone(names)
	for _, p := range prefixes {
		has = append(has, p+"…")
	}
	return fmt.Errorf("the %s target has no setting %s; it has %s", target, strings.Join(unknown, ", "), strings.Join(has, ", "))
}

// Get returns a setting, or def when it is absent.
func (s Settings) Get(key, def string) string {
	if v, ok := s[key]; ok {
		return v
	}
	return def
}

// Output is what a target makes of a release, as files: the CLI writes them
// under -o, or streams those marked Stream to stdout.
type Output struct {
	Files []File
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
