// Package docker is Docker engines as a target: one, or the hosts the
// environment names, each its own engine. A key becomes a container per
// instance — <installation>-<key>, or <installation>-<key>-<i> for scale:
// replicas. On one engine the containers reach each other by name on the
// installation's network; across hosts the operator says which hosts a key
// runs on, instance i on the i-th, and a neighbour reaches an instance at its
// host's address and the port the component says, which the host publishes.
// A container's config files are kept in a volume of its own on its host,
// mounted file by file; its claims are named volumes, its libraries image
// mounts. The target's half of the platform is replicas, hosts and publish.
// It performs steps through the docker CLI; `pack template` writes the
// containers and their files, and, asked to, a compose file per host.
package docker

import (
	_ "embed"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

//go:embed platform.json
var platformSchema []byte

// Target is Docker engines as a target. Of the environment's set it reads
//
//   - hosts_<name>_address, _host, _context: the hosts (hosts: {<name>:
//     {address, host, context}}) — how a neighbour reaches the host, an IP or
//     a DNS name, and docker's --host and --context for its engine; none —
//     the one engine of host and context;
//   - docker: the binary, default from PATH;
//   - network: the network on each engine, default the installation's name;
//   - state: a file that keeps the installation's record and the credentials
//     pack provisioned; default — the engine keeps them (the first host's);
//   - secrets_<name>_<key>: the values ${secret:<name>/<key>} names;
//   - compose: true — `pack template` writes compose files too;
//   - platform_<key>_…: its half of each key's platform.
type Target struct{}

func (Target) Name() string           { return "docker" }
func (Target) Capabilities() []string { return []string{target.CapExecute} }

func (Target) Configure(installation string, settings target.Settings) (target.Installation, error) {
	if err := settings.Check("docker", []string{"host", "context", "docker", "network", "network_subnet", "network_gateway", "state", "compose"},
		[]string{"hosts_", "secrets_", "platform_"}); err != nil {
		return nil, err
	}
	in := &Installation{
		installation: installation,
		network:      settings.Get("network", installation),
		settings:     settings,
		hosts:        map[string]*engine{},
	}
	bin := settings.Get("docker", "docker")
	for k, v := range settings {
		rest, ok := strings.CutPrefix(k, "hosts_")
		if !ok {
			continue
		}
		i := strings.LastIndexByte(rest, '_')
		if i <= 0 {
			return nil, fmt.Errorf("%s: a host is hosts: {<name>: {address, host, context}}", k)
		}
		name, field := rest[:i], rest[i+1:]
		e := in.hosts[name]
		if e == nil {
			e = &engine{name: name, bin: bin}
			in.hosts[name] = e
		}
		switch field {
		case "address":
			e.address = v
		case "host":
			e.host = v
		case "context":
			e.context = v
		default:
			return nil, fmt.Errorf("%s: a host has address, host, context", k)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(in.hosts)) {
		e := in.hosts[name]
		if e.address == "" {
			return nil, fmt.Errorf("hosts_%s_address: host %s has no address", name, name)
		}
		in.engines = append(in.engines, e)
	}
	if len(in.engines) == 0 {
		in.engines = []*engine{{host: settings.Get("host", ""), context: settings.Get("context", ""), bin: bin}}
	}
	in.state = engineState{in}
	if path := settings.Get("state", ""); path != "" {
		file, err := newFileState(path, installation)
		if err != nil {
			return nil, err
		}
		in.state = file
	}
	return in, nil
}

// Installation is the target bound to one installation on its engines.
type Installation struct {
	installation, network string
	settings              target.Settings
	state                 state
	// engines are the hosts by name, or the one engine; hosts by name.
	engines []*engine
	hosts   map[string]*engine
}

// spread: the environment names hosts, and a neighbour reaches an instance at
// its host's address.
func (in *Installation) spread() bool { return len(in.hosts) > 0 }

// Namespace: an engine has none.
func (in *Installation) Namespace() string { return "" }

func (in *Installation) PlatformSchema() []byte { return platformSchema }

// service is a key's name: its container's, its replicas' prefix, and on one
// engine an alias of every replica.
func (in *Installation) service(key string) string { return in.installation + "-" + key }

// fqdn is a container's name on one engine: <container>.<network>.
func (in *Installation) fqdn(name string) string { return name + "." + in.network }

// Resolve the key. On one engine it is <installation>-<key> on the
// installation's network, replicas <service>-<i> each reached by its name.
// Across hosts it runs on the hosts its platform names, instance i on the
// i-th — replicas, when not said, are as many as the hosts — and is reached
// at the host's address. The target's half of a key's platform is what the
// values say, under what the environment says of the key
// (platform_<key>_replicas, …_hosts, …_publish_<port>); installed, what the
// record kept.
func (in *Installation) Resolve(key, scale string, platform map[string]any, installed bool) (*render.Resolved, error) {
	own, err := in.platformOf(key, platform, installed)
	if err != nil {
		return nil, err
	}
	svc := in.serviceOf(key, own)
	hosts := hostsOf(own["hosts"])
	n, explicit := toInt(own["replicas"])
	pub, _ := own["publish"].(map[string]any)

	if !in.spread() {
		if len(hosts) > 0 {
			return nil, fmt.Errorf("%s: hosts: the environment names no hosts (hosts: {<name>: {address, host}})", key)
		}
		if !explicit {
			n, own["replicas"] = 1, int64(1)
		}
		// a neighbour reaches an instance at <container>.<network>: what the
		// engine's resolver answers a reverse query with, and what the
		// container calls itself (--domainname) — one canonical name, which
		// Kerberos turns into the principal on both ends
		res := &render.Resolved{Service: svc, Address: in.fqdn(svc), Platform: own}
		if h := strOf(own, "hostname"); h != "" && strings.Contains(h, ".") {
			res.Address = h
		}
		if scale == "replicas" {
			if n > 1 && len(pub) > 0 {
				return nil, fmt.Errorf("%s: publish binds a host port, which one container of %d replicas can have", key, n)
			}
			for i := range n {
				name := fmt.Sprintf("%s-%d", svc, i)
				res.Instances = append(res.Instances, render.Instance{Name: name, Address: in.fqdn(name)})
			}
		}
		return res, nil
	}

	if len(pub) > 0 {
		return nil, fmt.Errorf("%s: publish: across hosts every port is published as itself", key)
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("%s: say which hosts it runs on — platform: {%s: {hosts: [...]}}, of %s",
			key, key, strings.Join(slices.Sorted(maps.Keys(in.hosts)), ", "))
	}
	for i, h := range hosts {
		if in.hosts[h] == nil {
			return nil, fmt.Errorf("%s: hosts: %s is no host of the environment", key, h)
		}
		if slices.Contains(hosts[:i], h) {
			return nil, fmt.Errorf("%s: hosts: %s twice — two instances of one key on a host publish the same ports", key, h)
		}
	}
	if scale != "replicas" {
		if len(hosts) != 1 {
			return nil, fmt.Errorf("%s: hosts: a key that does not scale runs on one host, not %d", key, len(hosts))
		}
		own["replicas"] = int64(1)
		return &render.Resolved{Service: svc, Address: in.hosts[hosts[0]].address, Platform: own}, nil
	}
	if explicit && n != len(hosts) {
		return nil, fmt.Errorf("%s: %d replicas on %d hosts — an instance per host", key, n, len(hosts))
	}
	own["replicas"] = int64(len(hosts))
	res := &render.Resolved{Service: svc, Address: in.hosts[hosts[0]].address, Platform: own}
	for i, h := range hosts {
		res.Instances = append(res.Instances, render.Instance{Name: fmt.Sprintf("%s-%d", svc, i), Address: in.hosts[h].address})
	}
	return res, nil
}

func (in *Installation) platformOf(key string, platform map[string]any, installed bool) (map[string]any, error) {
	own := map[string]any{}
	for _, k := range append([]string{"replicas", "hosts", "publish"}, runtimeProps...) {
		if v, ok := platform[k]; ok {
			own[k] = v
		}
	}
	prefix := "platform_" + strings.ToLower(key) + "_"
	for k, v := range in.settings {
		if installed {
			break
		}
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		switch {
		case rest == "replicas":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			own["replicas"] = n
		case rest == "hosts":
			var hs []any
			for h := range strings.SplitSeq(v, ",") {
				if h = strings.TrimSpace(h); h != "" {
					hs = append(hs, h)
				}
			}
			own["hosts"] = hs
		case strings.HasPrefix(rest, "publish_"):
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			pub := map[string]any{}
			if m, ok := own["publish"].(map[string]any); ok {
				maps.Copy(pub, m)
			}
			pub[strings.TrimPrefix(rest, "publish_")] = n
			own["publish"] = pub
		case runtimeSetting(own, rest, v):
		default:
			return nil, fmt.Errorf("%s: the docker platform has replicas, hosts, publish, %s", k, strings.Join(runtimeProps, ", "))
		}
	}
	return own, nil
}

func (in *Installation) ValidatePlatform(string, map[string]any) []string { return nil }

// engineOf is the engine instance i of a unit runs on.
func (in *Installation) engineOf(u render.Unit, i int) (*engine, error) {
	if !in.spread() {
		return in.engines[0], nil
	}
	hosts := hostsOf(u.Platform["hosts"])
	if i >= len(hosts) || in.hosts[hosts[i]] == nil {
		return nil, fmt.Errorf("%s: instance %d has no host", u.Key, i)
	}
	return in.hosts[hosts[i]], nil
}

func hostsOf(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, h := range x {
			if s, ok := h.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = x
	}
	return out
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}
