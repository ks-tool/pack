package sdk

import "github.com/ks-tool/pack/sdk/packschema"

// Platform mirrors the platform half of a component's values — what the
// operator sizes — so that a Go default value is typed. Its schema is not
// the author's and not the pack's: the key refers to the platform slot
// (packschema.PlatformRef), which the target a pack is rendered for fills;
// the value here is the default layered over it. Placement is not here: it
// is the operator's, in the words of the target.
type Platform struct {
	Replicas  int       `json:"replicas,omitempty"`
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
