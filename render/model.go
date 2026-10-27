// Package render is the core of the tool, and knows no target: a bundle and
// a values document become a Release — the selection and its order, every
// formula evaluated, every config file rendered, libraries resolved to
// images. Where it runs is a target's business (target/horchestra); the one
// thing render asks of a target is what a key's platform means — names,
// addresses, instances, limits (Target.Resolve) — because a neighbour's
// address ends up inside a config file.
package render

import (
	"io/fs"

	"github.com/ks-tool/pack/sdk/packschema"
)

// Release is one render, before any target: what installs, in what order,
// built from what. Everything in it is settled — formulas evaluated, files
// rendered, libraries resolved to images — and nothing in it names an object
// of a particular system; a target turns it into its own (target/horchestra:
// an ApplicationSet and config Secrets).
type Release struct {
	Installation string
	Pack         string
	Version      string
	// Rollout is the operator's, else the composition's; nil when neither says.
	Rollout *packschema.Rollout
	// Units are the selected keys in install order.
	Units []Unit
	// Platforms are the target's halves of the keys' platforms as the target
	// resolved them — kept in the record, so that this version is rendered
	// again as it was installed (Context.Installed).
	Platforms map[string]map[string]any
	Report    Report
}

// Unit is one key of the composition: one program, run from an image, as
// one instance or as Replicas of them.
type Unit struct {
	Key       string
	Component string
	// Replicas is 0 for a key that does not scale (scale: single) — one
	// instance, addressed as the key — and n ≥ 1 for scale: replicas.
	Replicas      int
	Image         string
	RestartPolicy string
	// Command and Args: the image's entrypoint and cmd (or x-pack.command),
	// then the annotated leaves in author order.
	Command []string
	Args    []string
	// Env are the variables the component's annotated leaves make, then the
	// platform's additive ones.
	Env   []EnvVar
	Ports []Port
	// Resources are the cgroup requests and limits, as the platform has them.
	Resources *Resources
	// Labels and Annotations are the platform's metadata.
	Labels      map[string]string
	Annotations map[string]string
	// Spread asks for one instance per host — the composition's spread.
	Spread bool
	// Platform is the key's platform as the effective document has it: the
	// pack's half (read into the fields above) and the target's (replicas,
	// placement …), which the render passes on and never reads.
	Platform  map[string]any
	Readiness *Readiness
	// Data are the volumes that outlive an instance (claims).
	Data []Claim
	// Libraries are images whose content is projected into the unit,
	// merged with what the unit's own image has at the path.
	Libraries []Library
	// Files are the rendered config files, ordered by path.
	Files []File
	// SecretFiles are files whose bytes are a secret's value, untouched.
	SecretFiles []SecretFile
}

type EnvVar struct {
	Name   string
	Value  string
	Secret *SecretRef // instead of Value
}

// SecretRef names a value the operator keeps outside the values document.
type SecretRef struct {
	Name string
	Key  string
}

type Port struct {
	Name string
	Port int
}

// Resources are cgroup settings: requests and limits by resource name.
type Resources struct {
	Requests map[string]string
	Limits   map[string]string
}

// Readiness is one probe, its port resolved to a number.
type Readiness struct {
	TCP  *TCPProbe
	HTTP *HTTPProbe
	Exec []string
}

type TCPProbe struct {
	Port int
}

type HTTPProbe struct {
	Port int
	Path string
}

type Claim struct {
	Name string
	Path string
	Size string
}

type Library struct {
	Name  string // the member
	Path  string
	Image string
}

// File is one rendered config file. Its bytes may carry ${secret:<name>/<key>}
// where the values held a reference: the target resolves them where the file
// lands, so a credential never passes through the render as plaintext.
type File struct {
	Path string
	Mode *fs.FileMode
	// Content is the file of a unit; nil for a per-instance file.
	Content []byte
	// Instances are a per-instance file's bytes, replica i at i; nil where a
	// replica's section was left empty and the file does not exist for it.
	Instances [][]byte
	// Secrets are the names the placeholders refer to, in order of appearance.
	Secrets []string
}

// PerInstance reports whether the file differs per replica.
func (f File) PerInstance() bool { return f.Instances != nil }

// SecretFile is a file whose bytes are a secret's value — a keytab, a
// keystore. Per-instance: the key is <key>.<index>, one per replica.
type SecretFile struct {
	Path        string
	Mode        *fs.FileMode
	Secret      SecretRef
	PerInstance bool
}

// Report is what came from where — for the operator's eyes.
type Report struct {
	Installation string                       `json:"installation"`
	Namespace    string                       `json:"namespace"`
	Pack         string                       `json:"pack"`
	Order        []string                     `json:"order"`
	Peers        map[string]map[string]string `json:"peers,omitempty"`
	Computed     map[string]any               `json:"computed,omitempty"` // instance pointer → value the mutator wrote
	Secrets      map[string][]string          `json:"secrets,omitempty"`  // a target's: config object name → files
}
