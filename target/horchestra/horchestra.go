package horchestra

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/render"
)

const (
	LabelPack         = "horchestra.io/pack"
	LabelPackVersion  = "horchestra.io/pack-version"
	LabelInstallation = "horchestra.io/installation"
	LabelComponent    = "horchestra.io/component"
	LabelSet          = "horchestra.io/application-set"
	SecretTypeConfig  = "horchestra.io/config"
)

// Context is where a set is installed: under what name, in which namespace.
type Context struct {
	Installation string
	Namespace    string
}

// Names is how horchestra names and reaches what it runs: a key is the
// service <installation>-<key>, replica i of it <service>-<i>, reached at
// <service>-<i>.<service>.<namespace>.
func (c Context) Names() render.Names { return names{c.Installation, c.Namespace} }

type names struct{ installation, namespace string }

func (n names) Service(key string) string { return n.installation + "-" + key }

func (n names) Instance(key string, i int) string { return fmt.Sprintf("%s-%d", n.Service(key), i) }

func (n names) Address(key string, i int) string {
	if i < 0 {
		return n.Service(key) + "." + n.namespace
	}
	return fmt.Sprintf("%s.%s.%s", n.Instance(key, i), n.Service(key), n.namespace)
}

func (n names) Namespace() string { return n.namespace }

// Render renders a bundle for horchestra: render.Render with its names, then
// Convert.
func Render(b *bundle.Bundle, v render.Values, ctx Context) (*Output, error) {
	rel, err := render.Render(b, v, render.Context{Installation: ctx.Installation, Names: ctx.Names()})
	if err != nil {
		return nil, err
	}
	return Convert(rel, ctx)
}

// Convert builds the set and its config secrets from a release.
func Convert(rel *render.Release, ctx Context) (*Output, error) {
	out := &Output{Set: ApplicationSet{
		APIVersion: "horchestra.io/v1", Kind: "ApplicationSet",
		Metadata: ObjectMeta{Name: ctx.Installation, Namespace: ctx.Namespace,
			Labels: map[string]string{LabelPack: rel.Pack}},
	}, Report: rel.Report}
	if len(rel.Version) > 0 {
		out.Set.Metadata.Labels[LabelPackVersion] = rel.Version
	}
	if rel.Rollout != nil {
		out.Set.Spec.Rollout = &Rollout{MaxUnavailable: rel.Rollout.MaxUnavailable}
	}
	out.Report.Secrets = map[string][]string{}
	for _, u := range rel.Units {
		app, secrets, err := application(u, ctx, out.Report.Secrets)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u.Key, err)
		}
		out.Set.Spec.Applications = append(out.Set.Spec.Applications, *app)
		out.Secrets = append(out.Secrets, secrets...)
	}
	return out, nil
}

// Credentials are the Secrets of the credentials render.Provision read.
func Credentials(creds []render.Credential, ctx Context) []Secret {
	var out []Secret
	for _, c := range creds {
		out = append(out, Secret{
			APIVersion: "horchestra.io/v1", Kind: "Secret",
			Metadata: ObjectMeta{Name: c.Name, Namespace: ctx.Namespace,
				Labels: map[string]string{LabelInstallation: ctx.Installation, LabelComponent: c.Key}},
			Type:       "Opaque",
			StringData: map[string]string{c.Leaf: c.Value},
		})
	}
	return out
}

func application(u render.Unit, ctx Context, report map[string][]string) (*NamedApplicationSpec, []Secret, error) {
	app := &NamedApplicationSpec{Name: u.Key}
	if u.Replicas > 0 {
		app.Scale = &Scale{Replicas: u.Replicas}
	}
	app.Spec.Image = u.Image
	if len(u.RestartPolicy) > 0 {
		app.Spec.Lifecycle = &Lifecycle{RestartPolicy: u.RestartPolicy}
	}
	app.Spec.Command, app.Spec.Args = u.Command, u.Args
	for _, e := range u.Env {
		ev := EnvVar{Name: e.Name, Value: e.Value}
		if e.Secret != nil {
			ev.SecretRef = &SecretRef{Name: e.Secret.Name, Key: e.Secret.Key}
		}
		app.Spec.Env = append(app.Spec.Env, ev)
	}
	for _, p := range u.Ports {
		app.Spec.Ports = append(app.Spec.Ports, Port{Name: p.Name, Port: p.Port})
	}
	if u.Resources != nil {
		app.Spec.Resources = &Resources{Requests: u.Resources.Requests, Limits: u.Resources.Limits}
	}

	// placement: horchestra's own words, read from the platform subtree
	pl := &Placement{}
	if pp, ok := u.Platform["placement"].(map[string]any); ok {
		pl.NodeSelector = strMap(pp["nodeSelector"])
		pl.Queue, _ = pp["queue"].(string)
		if pr, ok := toInt(pp["priority"]); ok {
			p32 := int32(pr)
			pl.Priority = &p32
		}
	}
	if u.Spread {
		pl.Affinity = &Affinity{WorkloadAntiAffinity: &WorkloadAffinity{Required: []AffinityTerm{{
			TopologyKey: "horchestra.io/hostname", LabelSelector: map[string]string{LabelSet: ctx.Installation, LabelComponent: u.Key}}}}}
	}
	if pl.NodeSelector != nil || len(pl.Queue) > 0 || pl.Priority != nil || pl.Affinity != nil {
		app.Spec.Placement = pl
	}

	if rd := u.Readiness; rd != nil {
		app.Spec.Readiness = &Readiness{Exec: rd.Exec}
		if rd.TCP != nil {
			app.Spec.Readiness.TCP = &TCPProbe{Port: rd.TCP.Port}
		}
		if rd.HTTP != nil {
			app.Spec.Readiness.HTTP = &HTTPProbe{Port: rd.HTTP.Port, Path: rd.HTTP.Path}
		}
	}

	// volumes: claims, libraries, config secrets, secret files
	for _, c := range u.Data {
		app.Spec.Volumes = append(app.Spec.Volumes, VolumeMount{MountPath: c.Path, Volume: VolumeSource{PV: &PVVolume{Size: c.Size}}})
	}
	for _, l := range u.Libraries {
		app.Spec.Volumes = append(app.Spec.Volumes, VolumeMount{MountPath: l.Path, Merge: true, Volume: VolumeSource{Image: &ImageVolume{Reference: l.Image}}})
	}
	secrets, mounts := configSecrets(u, ctx, report)
	app.Spec.Volumes = append(app.Spec.Volumes, mounts...)
	app.Spec.Volumes = append(app.Spec.Volumes, secretFileMounts(u)...)
	app.Metadata = &ChildMeta{Labels: map[string]string{LabelComponent: u.Key}}
	return app, secrets, nil
}

// configSecrets groups the unit's files by directory into Secrets, with the
// mounts that use them. A per-instance file is one key per replica,
// <file>.<index>, and the item picks the replica's own by ${horchestra.index}.
func configSecrets(u render.Unit, ctx Context, report map[string][]string) ([]Secret, []VolumeMount) {
	byDir := map[string][]render.File{}
	var dirs []string
	for _, f := range u.Files {
		dir := path.Dir(f.Path)
		if _, seen := byDir[dir]; !seen {
			dirs = append(dirs, dir)
		}
		byDir[dir] = append(byDir[dir], f)
	}
	sort.Strings(dirs)
	var secrets []Secret
	var mounts []VolumeMount
	for _, dir := range dirs {
		group := byDir[dir]
		sort.Slice(group, func(i, j int) bool { return group[i].Path < group[j].Path })
		data := map[string]string{}
		binary := map[string]string{}
		var items []KeyToPath
		var subst []string
		var names []string
		put := func(k string, b []byte) {
			if utf8.Valid(b) {
				data[k] = string(b)
			} else {
				binary[k] = base64.StdEncoding.EncodeToString(b)
			}
			names = append(names, k)
		}
		for _, f := range group {
			base := path.Base(f.Path)
			itemKey := base
			if f.PerInstance() {
				// the keys in the order the old per-file map sorted them: base.0, base.1, base.10, base.2 …
				var keys []string
				byKey := map[string][]byte{}
				for i, b := range f.Instances {
					if b != nil {
						k := fmt.Sprintf("%s.%d", base, i)
						keys = append(keys, k)
						byKey[k] = b
					}
				}
				sort.Strings(keys)
				for _, k := range keys {
					put(k, byKey[k])
				}
				itemKey = base + ".${horchestra.index}"
			} else {
				put(base, f.Content)
			}
			items = append(items, KeyToPath{Key: itemKey, Path: base, Mode: mode(f.Mode)})
			subst = union(subst, f.Secrets)
		}
		// A stable name — the installation, the key and the directory — so rendering again
		// updates the same object in place and the output is idempotent. What turns the update
		// into a restart is horchestra's: the set stamps the Secret's resourceVersion on the
		// children that mount it (horchestra.io/config-revision), and the node restarts on it.
		name := configSecretName(ctx.Installation, u.Key, dir)
		secrets = append(secrets, Secret{
			APIVersion: "horchestra.io/v1", Kind: "Secret",
			Metadata: ObjectMeta{Name: name, Namespace: ctx.Namespace,
				Labels: map[string]string{LabelInstallation: ctx.Installation, LabelComponent: u.Key}},
			Type: SecretTypeConfig, StringData: nilIfEmpty(data), Data: nilIfEmpty(binary),
		})
		sort.Strings(subst)
		mounts = append(mounts, VolumeMount{MountPath: dir, Merge: true, Volume: VolumeSource{Secret: &SecretVolume{Name: name, Items: items, Substitute: subst}}})
		report[name] = names
	}
	return secrets, mounts
}

// configSecretName is `<installation>-<key>-<directory>`, the directory as a DNS label:
// `/opt/kafka/config` → `prod-kafka-opt-kafka-config`. Readable, and the same on every run.
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

// secretFileMounts mounts each secret file's key at its path; files in one
// directory from one secret share a mount. Per-instance: the key is
// <key>.${horchestra.index}, resolved by the set controller.
func secretFileMounts(u render.Unit) []VolumeMount {
	type slot struct {
		dir, secret string
	}
	items := map[slot][]KeyToPath{}
	var order []slot
	for _, f := range u.SecretFiles {
		k := f.Secret.Key
		if f.PerInstance {
			k += ".${horchestra.index}"
		}
		s := slot{path.Dir(f.Path), f.Secret.Name}
		if _, seen := items[s]; !seen {
			order = append(order, s)
		}
		items[s] = append(items[s], KeyToPath{Key: k, Path: path.Base(f.Path), Mode: mode(f.Mode)})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].dir != order[j].dir {
			return order[i].dir < order[j].dir
		}
		return order[i].secret < order[j].secret
	})
	var mounts []VolumeMount
	for _, s := range order {
		its := items[s]
		sort.Slice(its, func(i, j int) bool { return its[i].Path < its[j].Path })
		mounts = append(mounts, VolumeMount{MountPath: s.dir, Merge: true, Volume: VolumeSource{Secret: &SecretVolume{Name: s.secret, Items: its}}})
	}
	return mounts
}

func mode(m *fs.FileMode) *int32 {
	if m == nil {
		return nil
	}
	v := int32(*m)
	return &v
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case int64:
		return int(x), true
	case json.Number:
		n, err := x.Int64()
		return int(n), err == nil
	}
	return 0, false
}

func strMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, e := range m {
		out[k], _ = e.(string)
	}
	return out
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			a = append(a, s)
		}
	}
	return a
}
