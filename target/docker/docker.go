// Package docker is a Docker engine as a target: one host, the installation's
// own network. A key becomes a container per instance — <installation>-<key>,
// or <installation>-<key>-<i> for scale: replicas, each also answering to
// <installation>-<key> on the network; its config files are copied into the
// container before it starts, its claims named volumes, its libraries image
// mounts. The target's half of the platform is replicas and publish. It
// performs steps through the docker CLI, which carries whatever host and
// context the environment names; `pack template` writes the same as a
// compose file.
package docker

import (
	_ "embed"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

//go:embed platform.json
var platformSchema []byte

// Target is a Docker engine as a target. Of the environment's set it reads
// host and context (docker's --host, --context), docker (the binary, default
// from PATH), network (default: the installation's name), secrets_<name>_<key>
// for the values ${secret:<name>/<key>} names, state (a file that keeps the
// installation's record and the credentials pack provisioned; default: the
// engine keeps them) and platform_<key>_… for its half of each key's
// platform.
type Target struct{}

func (Target) Name() string           { return "docker" }
func (Target) Capabilities() []string { return []string{target.CapExecute} }

func (Target) Configure(installation string, settings target.Settings) (target.Installation, error) {
	in := &Installation{
		installation: installation,
		network:      settings.Get("network", installation),
		host:         settings.Get("host", ""),
		context:      settings.Get("context", ""),
		settings:     settings,
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

// Installation is the target bound to one installation on one engine.
type Installation struct {
	installation, network string
	host, context         string
	settings              target.Settings
	state                 state
}

// Namespace: a Docker engine has none; the installation's scope is its network.
func (in *Installation) Namespace() string { return "" }

func (in *Installation) PlatformSchema() []byte { return platformSchema }

// service is a key's name on the network: the container of a single key, an
// alias of every replica of a scaled one.
func (in *Installation) service(key string) string { return in.installation + "-" + key }

// Resolve the key as <installation>-<key> on the installation's network; for
// scale: replicas, replicas containers <service>-<i>, each reached by its
// name. The target's half of a key's platform is what the values say, under
// what the environment says of the key (platform_<key>_replicas,
// …_publish_<port>), replicas 1 when neither does.
func (in *Installation) Resolve(key, scale string, platform map[string]any) (*render.Resolved, error) {
	own, err := in.platformOf(key, platform)
	if err != nil {
		return nil, err
	}
	svc := in.service(key)
	res := &render.Resolved{Service: svc, Address: svc, Platform: own}
	if scale == "replicas" {
		n, _ := toInt(own["replicas"])
		if pub, _ := own["publish"].(map[string]any); n > 1 && len(pub) > 0 {
			return nil, fmt.Errorf("%s: publish binds a host port, which one container of %d replicas can have", key, n)
		}
		for i := range n {
			name := fmt.Sprintf("%s-%d", svc, i)
			res.Instances = append(res.Instances, render.Instance{Name: name, Address: name})
		}
	}
	return res, nil
}

func (in *Installation) platformOf(key string, platform map[string]any) (map[string]any, error) {
	own := map[string]any{"replicas": int64(1)}
	for _, k := range []string{"replicas", "publish"} {
		if v, ok := platform[k]; ok {
			own[k] = v
		}
	}
	prefix := "platform_" + strings.ToLower(key) + "_"
	for k, v := range in.settings {
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
		default:
			return nil, fmt.Errorf("%s: the docker platform has replicas, publish", k)
		}
	}
	return own, nil
}

func (in *Installation) ValidatePlatform(string, map[string]any) []string { return nil }

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
