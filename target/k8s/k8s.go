// Package k8s is Kubernetes as a target. A key becomes a StatefulSet and a
// headless Service, <installation>-<key> (a Job when it runs to completion);
// its config files a Secret per directory, each file mounted over the image
// by subPath; its claims volumeClaimTemplates; its libraries image volumes.
// The target's half of the platform is replicas, nodeSelector,
// priorityClassName, storageClass. It performs steps through kubectl, which
// carries whatever authentication the kubeconfig names.
package k8s

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

// Target is Kubernetes as a target. Of the environment's set it reads
// namespace (default "default"), kubeconfig and context (kubectl's), kubectl
// (the binary, default from PATH), and platform_<key>_… for its half of
// each key's platform.
type Target struct{}

func (Target) Name() string           { return "k8s" }
func (Target) Capabilities() []string { return []string{target.CapExecute} }

func (Target) Configure(installation string, settings target.Settings) (target.Installation, error) {
	if err := settings.Check("k8s", []string{"namespace", "kubeconfig", "context", "kubectl"}, []string{"platform_"}); err != nil {
		return nil, err
	}
	return &Installation{
		installation: installation,
		namespace:    settings.Get("namespace", "default"),
		kubeconfig:   settings.Get("kubeconfig", ""),
		context:      settings.Get("context", ""),
		settings:     settings,
	}, nil
}

// Installation is the target bound to one installation in one namespace.
type Installation struct {
	installation, namespace string
	kubeconfig, context     string
	settings                map[string]string
}

func (in *Installation) Namespace() string { return in.namespace }

// service is a key's StatefulSet and Service name.
func (in *Installation) service(key string) string { return in.installation + "-" + key }

func (in *Installation) PlatformSchema() []byte { return platformSchema }

// Resolve the headless Service <installation>-<key> at
// <service>.<namespace>.svc; for scale: replicas, replicas pods <service>-<i>
// at <service>-<i>.<service>.<namespace>.svc. The target's half of a key's
// platform is what the values say, under what the environment says of the
// key (platform_<key>_replicas, …_nodeselector_<label>, …_priorityclassname,
// …_storageclass), replicas 1 when neither does; installed, what the record
// kept.
func (in *Installation) Resolve(key, scale string, platform map[string]any, installed bool) (*render.Resolved, error) {
	own, err := in.platformOf(key, platform, installed)
	if err != nil {
		return nil, err
	}
	svc := in.service(key)
	res := &render.Resolved{Service: svc, Address: svc + "." + in.namespace + ".svc", Platform: own}
	if scale == "replicas" {
		n, _ := toInt(own["replicas"])
		for i := range n {
			name := fmt.Sprintf("%s-%d", svc, i)
			res.Instances = append(res.Instances, render.Instance{Name: name, Address: name + "." + svc + "." + in.namespace + ".svc"})
		}
	}
	return res, nil
}

func (in *Installation) platformOf(key string, platform map[string]any, installed bool) (map[string]any, error) {
	own := map[string]any{"replicas": int64(1)}
	for _, k := range []string{"replicas", "nodeSelector", "priorityClassName", "storageClass"} {
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
		case rest == "priorityclassname":
			own["priorityClassName"] = v
		case rest == "storageclass":
			own["storageClass"] = v
		case strings.HasPrefix(rest, "nodeselector_"):
			sel := map[string]any{}
			if m, ok := own["nodeSelector"].(map[string]any); ok {
				maps.Copy(sel, m)
			}
			sel[strings.TrimPrefix(rest, "nodeselector_")] = v
			own["nodeSelector"] = sel
		default:
			return nil, fmt.Errorf("%s: the k8s platform has replicas, nodeSelector, priorityClassName, storageClass", k)
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
