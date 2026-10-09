// Package horchestra is the horchestra target: a render.Release becomes what
// horchestra executes — an ApplicationSet and the Secrets holding its config
// files. The output types are the contract of spec §6 — a superset of
// horchestra's own where phase 2 adds fields (merge/substitute on a secret
// mount, readiness, ${horchestra.index} in an item key).
package horchestra

import "github.com/ks-tool/pack/render"

// ApplicationSet is the set as the tool writes it.
type ApplicationSet struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Metadata   ObjectMeta         `json:"metadata"`
	Spec       ApplicationSetSpec `json:"spec"`
}

type ObjectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type ApplicationSetSpec struct {
	Applications []NamedApplicationSpec `json:"applications"`
	Rollout      *Rollout               `json:"rollout,omitempty"`
}

type Rollout struct {
	MaxUnavailable int `json:"maxUnavailable,omitempty"`
}

type NamedApplicationSpec struct {
	Name     string          `json:"name"`
	Scale    *Scale          `json:"scale,omitempty"`
	Metadata *ChildMeta      `json:"metadata,omitempty"`
	Spec     ApplicationSpec `json:"spec"`
}

type Scale struct {
	Replicas int `json:"replicas"`
}

type ChildMeta struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type ApplicationSpec struct {
	Image     string        `json:"image"`
	Command   []string      `json:"command,omitempty"`
	Args      []string      `json:"args,omitempty"`
	Env       []EnvVar      `json:"env,omitempty"`
	Ports     []Port        `json:"ports,omitempty"`
	Resources *Resources    `json:"resources,omitempty"`
	Lifecycle *Lifecycle    `json:"lifecycle,omitempty"`
	Placement *Placement    `json:"placement,omitempty"`
	Volumes   []VolumeMount `json:"volumes,omitempty"`
	Readiness *Readiness    `json:"readiness,omitempty"` // phase 2
}

type EnvVar struct {
	Name      string     `json:"name"`
	Value     string     `json:"value,omitempty"`
	SecretRef *SecretRef `json:"secretRef,omitempty"`
}

type SecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type Port struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type Lifecycle struct {
	RestartPolicy string `json:"restartPolicy,omitempty"`
}

type Placement struct {
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	Queue        string            `json:"queue,omitempty"`
	Priority     *int32            `json:"priority,omitempty"`
	Affinity     *Affinity         `json:"affinity,omitempty"`
}

type Affinity struct {
	WorkloadAntiAffinity *WorkloadAffinity `json:"workloadAntiAffinity,omitempty"`
}

type WorkloadAffinity struct {
	Required []AffinityTerm `json:"requiredDuringScheduling,omitempty"`
}

type AffinityTerm struct {
	TopologyKey   string            `json:"topologyKey"`
	LabelSelector map[string]string `json:"labelSelector,omitempty"`
}

type VolumeMount struct {
	MountPath string       `json:"mountPath"`
	Merge     bool         `json:"merge,omitempty"`
	Volume    VolumeSource `json:"volume"`
}

type VolumeSource struct {
	PV     *PVVolume     `json:"pv,omitempty"`
	Image  *ImageVolume  `json:"image,omitempty"`
	Secret *SecretVolume `json:"secret,omitempty"`
}

type PVVolume struct {
	Size string `json:"size"`
}

type ImageVolume struct {
	Reference string `json:"reference"`
}

type SecretVolume struct {
	Name       string      `json:"name"`
	Items      []KeyToPath `json:"items,omitempty"`
	Substitute []string    `json:"substitute,omitempty"` // phase 2: names whose ${secret:…} the node resolves
}

type KeyToPath struct {
	Key  string `json:"key"`
	Path string `json:"path"`
	Mode *int32 `json:"mode,omitempty"`
}

type Readiness struct {
	TCP  *TCPProbe  `json:"tcp,omitempty"`
	HTTP *HTTPProbe `json:"http,omitempty"`
	Exec []string   `json:"exec,omitempty"`
}

type TCPProbe struct {
	Port int `json:"port"`
}

type HTTPProbe struct {
	Port int    `json:"port"`
	Path string `json:"path"`
}

// Secret is a config Secret as the tool writes it: immutable, named by content.
type Secret struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   ObjectMeta        `json:"metadata"`
	Type       string            `json:"type"`
	Immutable  bool              `json:"immutable,omitempty"`
	StringData map[string]string `json:"stringData,omitempty"`
	Data       map[string]string `json:"data,omitempty"`
}

// Output is everything one render produced for horchestra.
type Output struct {
	Set     ApplicationSet
	Secrets []Secret
	Report  render.Report
}
