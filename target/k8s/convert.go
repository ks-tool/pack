package k8s

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

type obj = map[string]any

const (
	labelName      = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelPart      = "pack.horchestra.io/part" // config | credentials | workload | data | hook
	annoConfig     = "pack.horchestra.io/config"
)

// Convert writes a key's objects to <key>.yaml — its credentials, config
// Secrets, Service and workload, applied in that order — and the report.
// ${secret:<name>/<key>} placeholders stay in the config Secrets: applying,
// the provider resolves them from the cluster's Secrets (Execute); rendered
// for reading, they are what was rendered.
func (in *Installation) Convert(rel *render.Release, credentials []render.Credential) (*target.Output, error) {
	out := &target.Output{}
	for _, u := range rel.Units {
		objs, err := in.objects(u, credentialsOf(credentials, u.Key), nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u.Key, err)
		}
		b, err := stream(objs)
		if err != nil {
			return nil, err
		}
		out.Files = append(out.Files, target.File{Path: u.Key + ".yaml", Data: b, Mode: 0o600, Stream: true})
	}
	rep, err := yaml.Marshal(rel.Report)
	if err != nil {
		return nil, err
	}
	out.Files = append(out.Files, target.File{Path: "report.yaml", Data: rep, Mode: 0o644})
	return out, nil
}

func credentialsOf(all []render.Credential, key string) []render.Credential {
	var out []render.Credential
	for _, c := range all {
		if c.Key == key {
			out = append(out, c)
		}
	}
	return out
}

// stream is objects as one YAML stream.
func stream(objs []obj) ([]byte, error) {
	var b strings.Builder
	for i, o := range objs {
		y, err := yaml.Marshal(o)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteString("---\n")
		}
		b.Write(y)
	}
	return []byte(b.String()), nil
}

func (in *Installation) labels(key string) map[string]any {
	return obj{labelName: key, labelInstance: in.installation, labelManagedBy: "pack"}
}

func (in *Installation) meta(name, key, part string) obj {
	l := in.labels(key)
	l[labelPart] = part
	return obj{"name": name, "namespace": in.namespace, "labels": l}
}

// objects are a key's objects; resolve, when set, turns a config Secret's
// placeholders into the values they name.
func (in *Installation) objects(u render.Unit, creds []render.Credential, resolve func(string) (string, error)) ([]obj, error) {
	svc := in.service(u.Key)
	var objs []obj
	for _, c := range creds {
		objs = append(objs, obj{"apiVersion": "v1", "kind": "Secret", "metadata": in.meta(c.Name, u.Key, "credentials"),
			"type": "Opaque", "stringData": obj{c.Leaf: c.Value}})
	}

	pod, secrets, checksum, err := in.podSpec(u, resolve)
	if err != nil {
		return nil, err
	}
	objs = append(objs, secrets...)

	ports := []any{}
	for _, p := range u.Ports {
		ports = append(ports, obj{"name": p.Name, "port": p.Port, "targetPort": p.Port})
	}
	service := obj{"apiVersion": "v1", "kind": "Service", "metadata": in.meta(svc, u.Key, "workload"),
		"spec": obj{"clusterIP": "None", "publishNotReadyAddresses": true, "selector": in.labels(u.Key)}}
	if len(ports) > 0 {
		service["spec"].(obj)["ports"] = ports
	}
	objs = append(objs, service)

	podLabels := in.labels(u.Key)
	maps.Copy(podLabels, toAny(u.Labels))
	annotations := toAny(u.Annotations)
	if checksum != "" {
		annotations[annoConfig] = checksum // a config change rolls the pods
	}
	tmpl := obj{"metadata": obj{"labels": podLabels}, "spec": pod}
	if len(annotations) > 0 {
		tmpl["metadata"].(obj)["annotations"] = annotations
	}

	if u.RestartPolicy == "Never" {
		pod["restartPolicy"] = "Never"
		for _, c := range u.Data {
			objs = append(objs, in.pvc(c.Name+"-"+svc, u, c))
			addVolume(pod, obj{"name": c.Name, "persistentVolumeClaim": obj{"claimName": c.Name + "-" + svc}})
		}
		objs = append(objs, obj{"apiVersion": "batch/v1", "kind": "Job", "metadata": in.meta(svc, u.Key, "workload"),
			"spec": obj{"backoffLimit": 0, "template": tmpl}})
		return objs, nil
	}
	replicas := u.Replicas
	if replicas == 0 {
		replicas = 1
	}
	spec := obj{"serviceName": svc, "replicas": replicas, "podManagementPolicy": "Parallel",
		"selector": obj{"matchLabels": in.labels(u.Key)}, "template": tmpl}
	var claims []any
	for _, c := range u.Data {
		claims = append(claims, in.pvc(c.Name, u, c))
	}
	if len(claims) > 0 {
		spec["volumeClaimTemplates"] = claims
	}
	objs = append(objs, obj{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": in.meta(svc, u.Key, "workload"), "spec": spec})
	return objs, nil
}

func (in *Installation) pvc(name string, u render.Unit, c render.Claim) obj {
	spec := obj{"accessModes": []any{"ReadWriteOnce"}, "resources": obj{"requests": obj{"storage": c.Size}}}
	if sc, ok := u.Platform["storageClass"].(string); ok && sc != "" {
		spec["storageClassName"] = sc
	}
	m := obj{"name": name}
	if name != c.Name { // a standalone claim (a Job's) is an object of its own; data outlives the key
		m = in.meta(name, u.Key, "data")
		return obj{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": m, "spec": spec}
	}
	return obj{"metadata": m, "spec": spec}
}

// podSpec is the key's pod: its container, its volumes and mounts, and the
// config Secrets the mounts take files from, with a checksum of their
// content.
func (in *Installation) podSpec(u render.Unit, resolve func(string) (string, error)) (pod obj, secrets []obj, checksum string, err error) {
	svc := in.service(u.Key)
	c := obj{"name": u.Key, "image": u.Image}
	if len(u.Command) > 0 {
		c["command"] = toList(u.Command)
	}
	if len(u.Args) > 0 {
		c["args"] = toList(u.Args)
	}
	var env []any
	for _, e := range u.Env {
		if e.Secret != nil {
			env = append(env, obj{"name": e.Name, "valueFrom": obj{"secretKeyRef": obj{"name": e.Secret.Name, "key": e.Secret.Key}}})
		} else {
			env = append(env, obj{"name": e.Name, "value": e.Value})
		}
	}
	var ports []any
	for _, p := range u.Ports {
		ports = append(ports, obj{"name": p.Name, "containerPort": p.Port})
	}
	if len(ports) > 0 {
		c["ports"] = ports
	}
	if r := u.Resources; r != nil {
		res := obj{}
		if len(r.Requests) > 0 {
			res["requests"] = toAny(r.Requests)
		}
		if len(r.Limits) > 0 {
			res["limits"] = toAny(r.Limits)
		}
		c["resources"] = res
	}
	if rd := u.Readiness; rd != nil {
		probe := obj{}
		switch {
		case rd.TCP != nil:
			probe["tcpSocket"] = obj{"port": rd.TCP.Port}
		case rd.HTTP != nil:
			probe["httpGet"] = obj{"port": rd.HTTP.Port, "path": rd.HTTP.Path}
		case len(rd.Exec) > 0:
			probe["exec"] = obj{"command": toList(rd.Exec)}
		}
		c["readinessProbe"] = probe
	}
	pod = obj{"containers": []any{c}}
	if ns, ok := u.Platform["nodeSelector"].(map[string]any); ok && len(ns) > 0 {
		pod["nodeSelector"] = ns
	}
	if pc, ok := u.Platform["priorityClassName"].(string); ok && pc != "" {
		pod["priorityClassName"] = pc
	}
	if u.Spread {
		pod["affinity"] = obj{"podAntiAffinity": obj{"requiredDuringSchedulingIgnoredDuringExecution": []any{
			obj{"topologyKey": "kubernetes.io/hostname", "labelSelector": obj{"matchLabels": in.labels(u.Key)}}}}}
	}

	var mounts []any
	perInstance := false
	for _, d := range u.Data {
		mounts = append(mounts, obj{"name": d.Name, "mountPath": d.Path})
	}
	for i, l := range u.Libraries {
		name := fmt.Sprintf("lib-%d", i)
		addVolume(pod, obj{"name": name, "image": obj{"reference": l.Image, "pullPolicy": "IfNotPresent"}})
		mounts = append(mounts, obj{"name": name, "mountPath": l.Path, "readOnly": true})
	}

	// config files: a Secret per directory, each file mounted by subPath over
	// what the image has there; a per-instance file is the pod's own key
	byDir := map[string][]render.File{}
	var dirs []string
	for _, f := range u.Files {
		d := path.Dir(f.Path)
		if _, seen := byDir[d]; !seen {
			dirs = append(dirs, d)
		}
		byDir[d] = append(byDir[d], f)
	}
	sort.Strings(dirs)
	sum := sha256.New()
	for n, dir := range dirs {
		name := configSecretName(in.installation, u.Key, dir)
		vol := fmt.Sprintf("cfg-%d", n)
		data, binary := obj{}, obj{}
		var items []any
		put := func(k string, b []byte) error {
			if resolve != nil {
				s, err := substitute(string(b), resolve)
				if err != nil {
					return fmt.Errorf("%s: %w", k, err)
				}
				b = []byte(s)
			}
			sum.Write([]byte(k))
			sum.Write(b)
			if utf8.Valid(b) {
				data[k] = string(b)
			} else {
				binary[k] = base64.StdEncoding.EncodeToString(b)
			}
			return nil
		}
		for _, f := range byDir[dir] {
			base := path.Base(f.Path)
			if f.PerInstance() {
				perInstance = true
				for i, b := range f.Instances {
					if b == nil {
						continue
					}
					k := fmt.Sprintf("%s.%d", base, i)
					if err := put(k, b); err != nil {
						return nil, nil, "", err
					}
					items = append(items, item(k, fmt.Sprintf("%s-%d/%s", svc, i, base), f.Mode))
				}
				mounts = append(mounts, obj{"name": vol, "mountPath": f.Path, "subPathExpr": "$(POD_NAME)/" + base, "readOnly": true})
				continue
			}
			if err := put(base, f.Content); err != nil {
				return nil, nil, "", err
			}
			items = append(items, item(base, base, f.Mode))
			mounts = append(mounts, obj{"name": vol, "mountPath": f.Path, "subPath": base, "readOnly": true})
		}
		s := obj{"apiVersion": "v1", "kind": "Secret", "metadata": in.meta(name, u.Key, "config"), "type": "Opaque"}
		if len(data) > 0 {
			s["stringData"] = data
		}
		if len(binary) > 0 {
			s["data"] = binary
		}
		secrets = append(secrets, s)
		addVolume(pod, obj{"name": vol, "secret": obj{"secretName": name, "items": items}})
	}
	if len(dirs) > 0 {
		checksum = hex.EncodeToString(sum.Sum(nil))[:16]
	}

	// secret files: the referenced Secret's key, its bytes as they are
	bySecret := map[string][]render.SecretFile{}
	var names []string
	for _, f := range u.SecretFiles {
		if _, seen := bySecret[f.Secret.Name]; !seen {
			names = append(names, f.Secret.Name)
		}
		bySecret[f.Secret.Name] = append(bySecret[f.Secret.Name], f)
	}
	sort.Strings(names)
	for n, sname := range names {
		vol := fmt.Sprintf("sec-%d", n)
		var items []any
		for _, f := range bySecret[sname] {
			base := path.Base(f.Path)
			if f.PerInstance {
				perInstance = true
				for i := range max(u.Replicas, 1) {
					items = append(items, item(fmt.Sprintf("%s.%d", f.Secret.Key, i), fmt.Sprintf("%s-%d/%s", svc, i, base), f.Mode))
				}
				mounts = append(mounts, obj{"name": vol, "mountPath": f.Path, "subPathExpr": "$(POD_NAME)/" + base, "readOnly": true})
				continue
			}
			items = append(items, item(f.Secret.Key, base, f.Mode))
			mounts = append(mounts, obj{"name": vol, "mountPath": f.Path, "subPath": base, "readOnly": true})
		}
		addVolume(pod, obj{"name": vol, "secret": obj{"secretName": sname, "items": items}})
	}
	if perInstance {
		env = append(env, obj{"name": "POD_NAME", "valueFrom": obj{"fieldRef": obj{"fieldPath": "metadata.name"}}})
	}
	if len(env) > 0 {
		c["env"] = env
	}
	if len(mounts) > 0 {
		c["volumeMounts"] = mounts
	}
	return pod, secrets, checksum, nil
}

// item projects a Secret's key to a path of its volume, with the file's mode.
func item(key, p string, mode *fs.FileMode) obj {
	it := obj{"key": key, "path": p}
	if mode != nil {
		it["mode"] = int(*mode)
	}
	return it
}

func addVolume(pod obj, v obj) {
	vols, _ := pod["volumes"].([]any)
	pod["volumes"] = append(vols, v)
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

// configSecretName is <installation>-<key>-<directory>, the directory as a
// DNS label: /opt/kafka/config → prod-kafka-opt-kafka-config.
func configSecretName(installation, key, dir string) string {
	slug := strings.Trim(strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, dir), "-")
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	return installation + "-" + key + "-" + slug
}

// substitute replaces ${secret:<name>/<key>} with what resolve says.
func substitute(s string, resolve func(string) (string, error)) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(s, "${secret:")
		if i < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		v, err := resolve(s[i+len("${secret:") : i+j])
		if err != nil {
			return "", err
		}
		b.WriteString(s[:i])
		b.WriteString(v)
		s = s[i+j+1:]
	}
}
