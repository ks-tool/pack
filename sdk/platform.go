package sdk

import "github.com/ks-tool/pack/sdk/packschema"

// Platform mirrors the pack's half of a component's platform — resources
// (cgroups: the same on Kubernetes, horchestra or a VM), env, metadata — so
// that a Go default value is typed. The key refers to the platform slot
// (packschema.PlatformRef): the pack's schema of these, joined to the
// target's. How many instances, where they go — replicas, placement — is
// the target's half: pack schedules nothing.
type Platform struct {
	Resources Resources `json:"resources,omitzero"`
	Env       []EnvVar  `json:"env,omitempty"`
	Metadata  *Metadata `json:"metadata,omitempty"`
}

type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type EnvVar struct {
	Name      string                `json:"name" jsonschema:"required,minLength=1"`
	Value     string                `json:"value,omitempty"`
	SecretRef *packschema.SecretRef `json:"secretRef,omitempty"`
}

type Metadata struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}
