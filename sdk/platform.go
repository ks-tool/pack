package sdk

import "github.com/ks-tool/pack/sdk/packschema"

// Platform mirrors the platform half of a component's values — what the
// operator sizes and places — so that a Go default value is typed. The
// schema itself is horchestra's (packschema.PlatformRef), not the author's;
// the value here is the default layered over it.
type Platform struct {
	Replicas  int        `json:"replicas,omitempty"`
	Resources Resources  `json:"resources,omitzero"`
	Placement *Placement `json:"placement,omitempty"`
	Env       []EnvVar   `json:"env,omitempty"`
	Metadata  *Metadata  `json:"metadata,omitempty"`
}

type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type Placement struct {
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	Queue        string            `json:"queue,omitempty"`
	Priority     *int32            `json:"priority,omitempty"`
	Spread       string            `json:"spread,omitempty" jsonschema:"enum=none,enum=host"`
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
