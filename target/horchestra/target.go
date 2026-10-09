package horchestra

import (
	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

// Target is horchestra as a target; of the environment's set it reads
// `namespace` (default "default").
type Target struct{}

func (Target) Name() string { return "horchestra" }

func (t Target) Configure(installation string, settings target.Settings) (target.Installation, error) {
	ctx := Context{Installation: installation, Namespace: settings.Get("namespace", "default")}
	return bound{names{ctx.Installation, ctx.Namespace}, ctx}, nil
}

// bound is horchestra bound to one installation, as a target.Installation.
type bound struct {
	names
	ctx Context
}

// Convert writes the set, the Secrets — provisioned credentials first, then
// the config Secrets — and the report.
func (i bound) Convert(rel *render.Release, credentials []render.Credential) (*target.Output, error) {
	c := i.ctx
	out, err := Convert(rel, c)
	if err != nil {
		return nil, err
	}
	set, err := yaml.Marshal(out.Set)
	if err != nil {
		return nil, err
	}
	files := []target.File{{Path: "applicationset.yaml", Data: set, Mode: 0o644, Stream: true}}
	for _, s := range append(Credentials(credentials, c), out.Secrets...) {
		b, err := yaml.Marshal(s)
		if err != nil {
			return nil, err
		}
		files = append(files, target.File{Path: "secrets/" + s.Metadata.Name + ".yaml", Data: b, Mode: 0o600, Stream: true})
	}
	rep, err := yaml.Marshal(out.Report)
	if err != nil {
		return nil, err
	}
	files = append(files, target.File{Path: "report.yaml", Data: rep, Mode: 0o644})
	return &target.Output{Files: files}, nil
}

var _ target.Target = Target{}
var _ target.Installation = bound{}
