package docker

import (
	"fmt"
	"io/fs"
	"path"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

type obj = map[string]any

// Convert writes the release as the containers it runs on their engines:
// containers.yaml — each container's host, image, command, env, ports, mounts
// — and its config files under files/<container>/, byte for byte,
// ${secret:<name>/<key>} as it was rendered; the credentials provisioned for
// it as secrets.env, PACK_SECRET_<NAME>_<KEY>=…; and the report. With the
// setting compose, a compose file per engine too — compose.yaml, or
// compose.<host>.yaml across hosts — whose services bind those files from
// files/: the output directory is the bundle, `docker compose -f
// compose.<host>.yaml --env-file secrets.env up` on the host, the variables
// carrying the env's secrets and the secret files'. Steps — hooks, order,
// waits — are pack's and are in none of it. Two containers that publish one
// port on one host are refused.
func (in *Installation) Convert(rel *render.Release, credentials []render.Credential) (*target.Output, error) {
	out := &target.Output{}
	var all []container
	for _, u := range rel.Units {
		cs, err := in.containers(u)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u.Key, err)
		}
		all = append(all, cs...)
	}
	if err := portsFree(all); err != nil {
		return nil, err
	}

	var specs []any
	for _, c := range all {
		specs = append(specs, in.spec(c))
		for _, f := range c.Files {
			out.Files = append(out.Files, target.File{Path: filePath(c, f.Path), Data: f.Data, Mode: 0o600})
		}
	}
	doc := obj{"installation": in.installation, "containers": specs}
	if in.spread() {
		hosts := obj{}
		for name, e := range in.hosts {
			hosts[name] = e.address
		}
		doc["hosts"] = hosts
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	out.Files = append([]target.File{{Path: "containers.yaml", Data: b, Mode: 0o644, Stream: true}}, out.Files...)

	if in.settings.Get("compose", "") == "true" {
		for _, e := range in.engines {
			var cs []container
			for _, c := range all {
				if c.Engine == e {
					cs = append(cs, c)
				}
			}
			if len(cs) == 0 {
				continue
			}
			b, err := yaml.Marshal(in.compose(cs))
			if err != nil {
				return nil, err
			}
			name := "compose.yaml"
			if e.name != "" {
				name = "compose." + e.name + ".yaml"
			}
			out.Files = append(out.Files, target.File{Path: name, Data: b, Mode: 0o644})
		}
	}

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

// portsFree refuses two containers that publish one port on one engine.
func portsFree(cs []container) error {
	taken := map[string]string{}
	for _, c := range cs {
		for _, p := range c.Ports {
			host, ok := c.Publish[p.Name]
			if !ok {
				continue
			}
			k := fmt.Sprintf("%p/%d", c.Engine, host)
			if other, ok := taken[k]; ok {
				where := "the engine"
				if c.Engine.name != "" {
					where = "host " + c.Engine.name
				}
				return fmt.Errorf("%s and %s both publish port %d on %s", other, c.Name, host, where)
			}
			taken[k] = c.Name
		}
	}
	return nil
}

// filePath is where a container's file is in the output: files/<container>/<path>.
func filePath(c container, p string) string {
	return path.Join("files", c.Name, strings.TrimPrefix(p, "/"))
}

// spec is a container as containers.yaml has it.
func (in *Installation) spec(c container) obj {
	s := obj{"name": c.Name, "image": c.Image, "restart": c.Restart, "labels": toAny(c.Labels)}
	if c.Engine.name != "" {
		s["host"] = c.Engine.name
	} else {
		s["network"] = in.network
	}
	if len(c.Aliases) > 0 {
		s["aliases"] = toList(c.Aliases)
	}
	if len(c.Entrypoint) > 0 {
		s["entrypoint"] = toList(c.Entrypoint)
	}
	if len(c.Cmd) > 0 {
		s["command"] = toList(c.Cmd)
	}
	if len(c.Env) > 0 {
		env := obj{}
		for _, e := range c.Env {
			if e.Secret != nil {
				env[e.Name] = "${secret:" + e.Secret.Name + "/" + e.Secret.Key + "}"
			} else {
				env[e.Name] = e.Value
			}
		}
		s["env"] = env
	}
	if ports := publishes(c); len(ports) > 0 {
		s["ports"] = ports
	}
	res := obj{}
	if c.CPUs != "" {
		res["cpus"] = c.CPUs
	}
	if c.CPUShares > 0 {
		res["cpuShares"] = c.CPUShares
	}
	if c.Memory > 0 {
		res["memory"] = c.Memory
	}
	if c.MemReservation > 0 {
		res["memoryReservation"] = c.MemReservation
	}
	if len(res) > 0 {
		s["resources"] = res
	}
	if len(c.Health) > 0 {
		s["healthcheck"] = toList(c.Health)
	}
	var mounts []any
	for _, v := range c.Volumes {
		mounts = append(mounts, obj{"volume": v.Source, "target": v.Target})
	}
	for _, l := range c.Libraries {
		mounts = append(mounts, obj{"image": l.Source, "target": l.Target})
	}
	for _, f := range c.Files {
		mounts = append(mounts, withMode(obj{"file": filePath(c, f.Path), "target": f.Path}, f.Mode))
	}
	for _, f := range c.SecretFiles {
		mounts = append(mounts, withMode(obj{"secret": f.Ref.Name + "/" + f.Ref.Key, "target": f.Path}, f.Mode))
	}
	if len(mounts) > 0 {
		s["mounts"] = mounts
	}
	return s
}

// compose is the containers of one engine as a compose file: a service per
// container, its files bound from files/, a secret file a secret of the
// variable that carries it.
func (in *Installation) compose(cs []container) obj {
	services, secrets, volumes := obj{}, obj{}, obj{}
	for _, c := range cs {
		s := obj{"image": c.Image, "container_name": c.Name, "hostname": c.Name, "restart": c.Restart, "labels": toAny(c.Labels)}
		if len(c.Entrypoint) > 0 {
			s["entrypoint"] = escapeList(c.Entrypoint)
		}
		if len(c.Cmd) > 0 {
			s["command"] = escapeList(c.Cmd)
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
		if ports := publishes(c); len(ports) > 0 {
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
			s["healthcheck"] = obj{"test": escapeList(c.Health), "interval": healthInterval, "timeout": healthTimeout, "retries": healthRetries}
		}
		var vols []any
		for _, v := range c.Volumes {
			vols = append(vols, obj{"type": "volume", "source": v.Source, "target": v.Target})
			volumes[v.Source] = obj{"name": v.Source}
		}
		for _, l := range c.Libraries {
			vols = append(vols, obj{"type": "image", "source": l.Source, "target": l.Target, "read_only": true})
		}
		for _, f := range c.Files {
			vols = append(vols, obj{"type": "bind", "source": "./" + filePath(c, f.Path), "target": f.Path, "read_only": true})
		}
		if len(vols) > 0 {
			s["volumes"] = vols
		}
		var secs []any
		for _, f := range c.SecretFiles {
			id := strings.ToLower(variable(f.Ref))
			secrets[id] = obj{"environment": variable(f.Ref)}
			secs = append(secs, obj{"source": id, "target": f.Path}) // a mode is swarm's
		}
		if len(secs) > 0 {
			s["secrets"] = secs
		}
		services[strings.TrimPrefix(c.Name, in.installation+"-")] = s
	}
	doc := obj{"name": in.installation, "services": services, "networks": obj{"default": obj{"name": in.network}}}
	if len(secrets) > 0 {
		doc["secrets"] = secrets
	}
	if len(volumes) > 0 {
		doc["volumes"] = volumes
	}
	return doc
}

// publishes are a container's published ports, <host port>:<port>.
func publishes(c container) []any {
	var ports []any
	for _, p := range c.Ports {
		if host, ok := c.Publish[p.Name]; ok {
			ports = append(ports, fmt.Sprintf("%d:%d", host, p.Port))
		}
	}
	return ports
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

func escapeList(s []string) []any {
	out := make([]any, len(s))
	for i, e := range s {
		out[i] = escape(e)
	}
	return out
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
