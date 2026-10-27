package docker

import (
	"fmt"
	"io/fs"
	"path"
	"strings"
	"unicode/utf8"

	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

type obj = map[string]any

// Convert writes the release as compose.yaml — a service per container, its
// config files as configs with their content, a secret file as a secret of
// the variable PACK_SECRET_<NAME>_<KEY> — and the report; the credentials
// provisioned for it as secrets.env, those variables, for `docker compose
// --env-file`. ${secret:<name>/<key>} in a file is ${PACK_SECRET_<NAME>_<KEY>}
// there, which compose interpolates, every other $ escaped; applying, the
// provider resolves them itself (Execute). Steps — hooks, order, waits — are
// pack's and are not in it.
func (in *Installation) Convert(rel *render.Release, credentials []render.Credential) (*target.Output, error) {
	out := &target.Output{}
	services, configs, secrets, volumes := obj{}, obj{}, obj{}, obj{}
	for _, u := range rel.Units {
		cs, err := in.containers(u)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u.Key, err)
		}
		for _, c := range cs {
			name := strings.TrimPrefix(c.Name, in.installation+"-")
			svc, extra := in.composeService(c, name, configs, secrets, volumes)
			services[name] = svc
			out.Files = append(out.Files, extra...)
		}
	}
	doc := obj{"name": in.installation, "services": services,
		"networks": obj{"default": obj{"name": in.network}}}
	for k, m := range map[string]obj{"configs": configs, "secrets": secrets, "volumes": volumes} {
		if len(m) > 0 {
			doc[k] = m
		}
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	out.Files = append([]target.File{{Path: "compose.yaml", Data: b, Mode: 0o644, Stream: true}}, out.Files...)

	if len(credentials) > 0 {
		var env strings.Builder
		for _, c := range credentials {
			fmt.Fprintf(&env, "%s=%s\n", variable(render.SecretRef{Name: c.Name, Key: c.Leaf}), c.Value)
		}
		out.Files = append(out.Files, target.File{Path: "secrets.env", Data: []byte(env.String()), Mode: 0o600})
	}
	rep, err := yaml.Marshal(rel.Report)
	if err != nil {
		return nil, err
	}
	out.Files = append(out.Files, target.File{Path: "report.yaml", Data: rep, Mode: 0o644})
	return out, nil
}

// composeService is c as a compose service; its configs, secrets and volumes
// go to the document's, and a file that is not text to files/ beside it.
func (in *Installation) composeService(c container, name string, configs, secrets, volumes obj) (obj, []target.File) {
	s := obj{"image": c.Image, "container_name": c.Name, "hostname": c.Name, "restart": c.Restart, "labels": toAny(c.Labels)}
	if len(c.Entrypoint) > 0 {
		s["entrypoint"] = toList(c.Entrypoint)
	}
	if len(c.Cmd) > 0 {
		s["command"] = toList(c.Cmd)
	}
	if len(c.Aliases) > 0 {
		s["networks"] = obj{"default": obj{"aliases": toList(c.Aliases)}}
	}
	if len(c.Env) > 0 {
		env := obj{}
		for _, e := range c.Env {
			if e.Secret != nil {
				env[e.Name] = "${" + variable(*e.Secret) + "}"
			} else {
				env[e.Name] = escape(e.Value)
			}
		}
		s["environment"] = env
	}
	var ports []any
	for _, p := range c.Ports {
		if host, ok := c.Publish[p.Name]; ok {
			ports = append(ports, fmt.Sprintf("%d:%d", host, p.Port))
		}
	}
	if len(ports) > 0 {
		s["ports"] = ports
	}
	if c.CPUs != "" {
		s["cpus"] = c.CPUs
	}
	if c.CPUShares > 0 {
		s["cpu_shares"] = c.CPUShares
	}
	if c.Memory > 0 {
		s["mem_limit"] = c.Memory
	}
	if c.MemReservation > 0 {
		s["mem_reservation"] = c.MemReservation
	}
	if len(c.Health) > 0 {
		s["healthcheck"] = obj{"test": toList(c.Health), "interval": healthInterval, "timeout": healthTimeout, "retries": healthRetries}
	}
	var vols []any
	for _, v := range c.Volumes {
		vols = append(vols, obj{"type": "volume", "source": v.Source, "target": v.Target})
		volumes[v.Source] = obj{"name": v.Source}
	}
	for _, l := range c.Libraries {
		vols = append(vols, obj{"type": "image", "source": l.Source, "target": l.Target, "read_only": true})
	}
	if len(vols) > 0 {
		s["volumes"] = vols
	}
	var files []target.File
	var cfgs []any
	for n, f := range c.Files {
		id := fmt.Sprintf("%s-%d-%s", name, n, path.Base(f.Path))
		if utf8.Valid(f.Data) {
			configs[id] = obj{"content": interpolate(string(f.Data))}
		} else {
			p := path.Join("files", name, f.Path)
			configs[id] = obj{"file": "./" + p}
			files = append(files, target.File{Path: p, Data: f.Data, Mode: 0o600})
		}
		cfgs = append(cfgs, withMode(obj{"source": id, "target": f.Path}, f.Mode))
	}
	if len(cfgs) > 0 {
		s["configs"] = cfgs
	}
	var secs []any
	for _, f := range c.SecretFiles {
		id := strings.ToLower(variable(f.Ref))
		secrets[id] = obj{"environment": variable(f.Ref)}
		secs = append(secs, withMode(obj{"source": id, "target": f.Path}, f.Mode))
	}
	if len(secs) > 0 {
		s["secrets"] = secs
	}
	return s, files
}

// withMode sets a file's mode, in octal as compose reads it.
func withMode(o obj, mode *fs.FileMode) obj {
	if mode != nil {
		o["mode"] = fmt.Sprintf("0%o", *mode)
	}
	return o
}

// variable is the compose variable a secret's value is passed in:
// PACK_SECRET_<NAME>_<KEY>, upper case, [A-Z0-9_].
func variable(ref render.SecretRef) string {
	return "PACK_SECRET_" + strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r >= 'a' && r <= 'z':
			return r - ('a' - 'A')
		}
		return '_'
	}, ref.Name+"_"+ref.Key)
}

// escape keeps compose from interpolating a value: $ is $$.
func escape(s string) string { return strings.ReplaceAll(s, "$", "$$") }

// interpolate is a file's content for compose: ${secret:<name>/<key>} the
// variable that carries it, every other $ escaped.
func interpolate(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "${secret:")
		j := -1
		if i >= 0 {
			j = strings.IndexByte(s[i:], '}')
		}
		if i < 0 || j < 0 {
			b.WriteString(escape(s))
			return b.String()
		}
		name, key, _ := strings.Cut(s[i+len("${secret:"):i+j], "/")
		b.WriteString(escape(s[:i]))
		b.WriteString("${" + variable(render.SecretRef{Name: name, Key: key}) + "}")
		s = s[i+j+1:]
	}
}

func toList(s []string) []any {
	out := make([]any, len(s))
	for i, e := range s {
		out[i] = e
	}
	return out
}

func toAny(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}
