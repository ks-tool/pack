// Package packschema is the vocabulary a component or a composition speaks on
// top of JSON Schema — the x-pack-* keywords — and the generator that turns a
// Go default value into such a schema.
//
// A validator ignores keywords it does not know, which is what lets the
// vocabulary live inside a standard schema; it is also why nothing but this
// package checks the keywords. The types here are that check: an annotation is
// decoded into them strictly, and a key they do not have is a refusal.
package packschema

// Root is `x-pack` on the root of a component schema: what the component asks of
// the system around it and how it changes versions.
type Root struct {
	// Scale is a fact about the application: single — one instance, addressed
	// as the key — or replicas, any number the target runs. How many is the
	// target's.
	Scale     string            `json:"scale,omitempty" jsonschema:"enum=single,enum=replicas"`
	Readiness *Readiness        `json:"readiness,omitempty"`
	Peers     map[string]Peer   `json:"peers,omitempty"`    // role → what kind of neighbour
	Requires  map[string]string `json:"requires,omitempty"` // library name → version constraint
	Upgrade   *Upgrade          `json:"upgrade,omitempty"`
	Hooks     []Hook            `json:"hooks,omitempty"`
	Command   []string          `json:"command,omitempty"` // replaces the image's entrypoint+cmd wholesale
	// RestartPolicy Never makes the component a run-to-completion step of the
	// set (a bootstrap, a schema migration): it runs once, in order, and the
	// set waits for it to finish rather than to become ready.
	RestartPolicy string `json:"restartPolicy,omitempty" jsonschema:"enum=Always,enum=OnFailure,enum=Never"`

	// Composition-only fields — `x-pack` on the root of a pack schema.
	Savepoints []string `json:"savepoints,omitempty"`
	Rollout    *Rollout `json:"rollout,omitempty"`
}

// Key is `x-pack` on a key of a composition: what the component under that key
// cannot know about the system.
type Key struct {
	Peers   map[string]string `json:"peers,omitempty"` // role → key; only needed when the type is ambiguous
	After   []string          `json:"after,omitempty"` // extra ordering edges beyond the peers graph
	Spread  bool              `json:"spread,omitempty"`
	Minimum map[string]string `json:"minimum,omitempty"` // lower bound for platform.resources.requests
}

type Readiness struct {
	TCP  string   `json:"tcp,omitempty"`  // a port name
	HTTP string   `json:"http,omitempty"` // "<port>/<path>"
	Exec []string `json:"exec,omitempty"`
}

type Peer struct {
	Schema   string `json:"schema" jsonschema:"required,minLength=1"` // io.horchestra.name of the neighbour's component
	Required bool   `json:"required,omitempty"`                       // the component does not start without it
}

type Upgrade struct {
	From string `json:"from" jsonschema:"required"` // version constraint on the version being upgraded from
}

// Hook is one step the component contributes to the saga of its own
// installation or upgrade: an install hook runs once around the first apply
// (`hdfs namenode -format` before it, a schema bootstrap after), an upgrade or
// downgrade hook around a version transition.
type Hook struct {
	Name       string   `json:"name" jsonschema:"required,minLength=1"`
	Phase      string   `json:"phase" jsonschema:"required,enum=preInstall,enum=postInstall,enum=preUpgrade,enum=postUpgrade,enum=preDowngrade,enum=postDowngrade"`
	When       *When    `json:"when,omitempty"`  // nil: on any image change; install hooks may constrain `to` only
	Image      string   `json:"image,omitempty"` // component hook: new (default) | old; composition hook: a member's name
	Command    []string `json:"command" jsonschema:"required,minItems=1"`
	Exclusive  bool     `json:"exclusive,omitempty"`
	Timeout    string   `json:"timeout,omitempty"`
	Idempotent bool     `json:"idempotent,omitempty"`
	Compensate *Step    `json:"compensate,omitempty"`
	For        []string `json:"for,omitempty"` // composition hooks only: which keys it is about
}

type Step struct {
	Image   string   `json:"image,omitempty" jsonschema:"enum=new,enum=old"`
	Command []string `json:"command" jsonschema:"required,minItems=1"`
}

type When struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

type Rollout struct {
	MaxUnavailable int `json:"maxUnavailable,omitempty"`
}

// File is `x-pack-file` on a subtree: the subtree, whole, becomes one file.
type File struct {
	Path        string `json:"path" jsonschema:"required,pattern=^/"`
	Format      string `json:"format" jsonschema:"required,enum=json,enum=yaml,enum=properties,enum=env,enum=shell,enum=ini,enum=flagfile,enum=lines,enum=hadoop-xml,enum=text,enum=template,enum=secret"`
	Template    string `json:"template,omitempty"`
	ParseAs     string `json:"parseAs,omitempty"`
	Mode        string `json:"mode,omitempty" jsonschema:"pattern=^0?[0-7]{3}$"`
	PerInstance bool   `json:"perInstance,omitempty"`
}

// Arg is `x-pack-arg` on a leaf: the leaf lands in argv.
type Arg struct {
	Flag      string `json:"flag" jsonschema:"required,minLength=1"`
	Prefix    string `json:"prefix,omitempty"`
	Each      bool   `json:"each,omitempty"`
	OmitEmpty bool   `json:"omitEmpty,omitempty"`
}

// Formats every emitter knows, in the order the spec lists them.
var Formats = []string{"json", "yaml", "properties", "env", "shell", "ini", "flagfile", "lines", "hadoop-xml", "text", "template"}

// FormatSecret is `x-pack-file` on a password leaf: not an emitter — the
// referenced secret's key is mounted at the path, bytes untouched (a keytab, a
// keystore, a certificate).
const FormatSecret = "secret"

// Phases a hook may belong to.
var Phases = []string{"preInstall", "postInstall", "preUpgrade", "postUpgrade", "preDowngrade", "postDowngrade"}

// IsInstall says whether the phase belongs to the first installation rather
// than to a version transition.
func IsInstall(phase string) bool { return phase == "preInstall" || phase == "postInstall" }

// The two foreign $ref schemes a component schema may use.
const (
	PlatformRef = "urn:pack:platform"
	PackScheme  = "pack://"
)

// SecretRef is the only thing a `format: password` leaf may hold.
type SecretRef struct {
	Name string `json:"name" jsonschema:"required,minLength=1"`
	Key  string `json:"key" jsonschema:"required,minLength=1"`
}

// Secret is the Go type of a password leaf: it marshals as {secretRef: …} and
// its schema is a string with format password (the validator swaps that for
// $defs/secretRef, see the loader).
type Secret struct {
	SecretRef *SecretRef `json:"secretRef,omitempty"`
}

func (s Secret) IsZero() bool {
	return s.SecretRef == nil
}

// Claim is one volume a component needs: where it is mounted is the
// component's fact, how big and on what class is the operator's setting.
type Claim struct {
	MountPath string `json:"mountPath" jsonschema:"required,pattern=^/" pack:"const"`
	Size      string `json:"size" pack:"default"`
}
