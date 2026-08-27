package render

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/emit"
	"github.com/ks-tool/pack/sdk/packschema"
	"github.com/ks-tool/pack/sdk/schema"
)

const (
	LabelPack         = "horchestra.io/pack"
	LabelPackVersion  = "horchestra.io/pack-version"
	LabelInstallation = "horchestra.io/installation"
	LabelComponent    = "horchestra.io/component"
	LabelSet          = "horchestra.io/application-set"
	SecretTypeConfig  = "horchestra.io/config"
)

// convert builds the set and its config secrets from the effective document.
func (r *render) convert(v Values) (*Output, error) {
	out := &Output{Set: ApplicationSet{
		APIVersion: "horchestra.io/v1", Kind: "ApplicationSet",
		Metadata: ObjectMeta{Name: r.ctx.Installation, Namespace: r.ctx.Namespace,
			Labels: map[string]string{LabelPack: r.b.Name}},
	}}
	if len(r.b.Version) > 0 {
		out.Set.Metadata.Labels[LabelPackVersion] = r.b.Version
	}
	if v.Rollout != nil {
		out.Set.Spec.Rollout = &Rollout{MaxUnavailable: v.Rollout.MaxUnavailable}
	} else if r.b.Root.Rollout != nil {
		out.Set.Spec.Rollout = &Rollout{MaxUnavailable: r.b.Root.Rollout.MaxUnavailable}
	}
	for _, key := range r.order {
		app, secrets, err := r.component(key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		out.Set.Spec.Applications = append(out.Set.Spec.Applications, *app)
		out.Secrets = append(out.Secrets, secrets...)
	}
	out.Report = r.report
	return out, nil
}

// file is one x-pack-file found under a key, with the node it hangs on.
type file struct {
	ptr    string
	node   *schema.Node
	schema *schema.Schema
	lib    string // the library it comes from, "" for the component's own
}

func (r *render) component(key string) (*NamedApplicationSpec, []Secret, error) {
	comp := r.b.Components[r.b.KeyComponent[key]]
	keyDoc, _ := get(r.doc, "/"+key)
	km, _ := keyDoc.(map[string]any)
	if km == nil {
		km = map[string]any{}
	}
	ann, _ := r.b.Composition.Root.Properties[key].KeyAnnotation()
	app := &NamedApplicationSpec{Name: key}
	if n := r.replicas(key); n > 0 {
		app.Scale = &Scale{Replicas: n}
	}

	// image
	if len(comp.ImageRef) == 0 {
		return nil, nil, fmt.Errorf("component %s has no image reference (pack.yaml oci:// item or image.ref)", comp.Name)
	}
	app.Spec.Image = comp.ImageRef
	if len(comp.Root.RestartPolicy) > 0 {
		app.Spec.Lifecycle = &Lifecycle{RestartPolicy: comp.Root.RestartPolicy}
	}

	// argv: the image's entrypoint+cmd (or x-pack.command) ⊕ annotated leaves in author order
	if len(comp.Root.Command) > 0 {
		app.Spec.Command = comp.Root.Command
	} else if comp.Image != nil {
		app.Spec.Command = comp.Image.Config.Entrypoint
		app.Spec.Args = append([]string{}, comp.Image.Config.Cmd...)
	}
	var files []file
	var envs []EnvVar
	var libs []string
	err := r.b.Composition.Visit(func(ptr string, s *schema.Schema, n *schema.Node) error {
		if !strings.HasPrefix(ptr, "/"+key+"/") && ptr != "/"+key {
			return nil
		}
		rel := strings.TrimPrefix(ptr, "/"+key)
		if n.IsForeign() && strings.HasPrefix(n.Ref, packschema.PackScheme) && ptr != "/"+key {
			url, _ := schema.SplitRef(n.Ref)
			libs = append(libs, strings.TrimPrefix(url, packschema.PackScheme))
		}
		if n.Arg != nil {
			val, ok := get(km, rel)
			args, err := argv(n.Arg, val, ok)
			if err != nil {
				return fmt.Errorf("%s: %w", ptr, err)
			}
			app.Spec.Args = append(app.Spec.Args, args...)
		}
		if len(n.Env) > 0 {
			val, ok := get(km, rel)
			if ok && val != nil {
				e, err := envVar(n.Env, val)
				if err != nil {
					return fmt.Errorf("%s: %w", ptr, err)
				}
				envs = append(envs, e)
			}
		}
		if n.File != nil {
			f := file{ptr: ptr, node: n, schema: s}
			segs := schema.Split(rel)
			if len(segs) > 1 {
				if ln := comp.Schema.Root.Properties[segs[0]]; ln != nil && ln.IsForeign() && strings.HasPrefix(ln.Ref, packschema.PackScheme) {
					url, _ := schema.SplitRef(ln.Ref)
					f.lib = strings.TrimPrefix(url, packschema.PackScheme)
				}
			}
			files = append(files, f)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// env: annotated leaves, then the platform's additive ones
	platform, _ := km["platform"].(map[string]any)
	if pe, ok := platform["env"].([]any); ok {
		for _, e := range pe {
			em, _ := e.(map[string]any)
			name, _ := em["name"].(string)
			for _, existing := range envs {
				if existing.Name == name {
					return nil, nil, fmt.Errorf("platform.env %s: the component declares that variable", name)
				}
			}
			if sr, ok := em["secretRef"].(map[string]any); ok {
				envs = append(envs, EnvVar{Name: name, SecretRef: &SecretRef{Name: str(sr["name"]), Key: str(sr["key"])}})
			} else {
				envs = append(envs, EnvVar{Name: name, Value: str(em["value"])})
			}
		}
	}
	app.Spec.Env = envs

	// ports
	if pm, ok := km["ports"].(map[string]any); ok {
		for _, name := range sortedKeys(pm) {
			p, _ := toInt(pm[name])
			app.Spec.Ports = append(app.Spec.Ports, Port{Name: name, Port: p})
		}
	}

	// resources, placement
	if res, ok := platform["resources"].(map[string]any); ok {
		app.Spec.Resources = &Resources{Requests: strMap(res["requests"]), Limits: strMap(res["limits"])}
	}
	pl := &Placement{}
	spread := ann.Spread
	if pp, ok := platform["placement"].(map[string]any); ok {
		pl.NodeSelector = strMap(pp["nodeSelector"])
		pl.Queue = str(pp["queue"])
		if pr, ok := toInt(pp["priority"]); ok {
			p32 := int32(pr)
			pl.Priority = &p32
		}
		if str(pp["spread"]) == "host" {
			spread = true
		}
	}
	if spread {
		pl.Affinity = &Affinity{WorkloadAntiAffinity: &WorkloadAffinity{Required: []AffinityTerm{{
			TopologyKey: "horchestra.io/hostname", LabelSelector: map[string]string{LabelSet: r.ctx.Installation, LabelComponent: key}}}}}
	}
	if pl.NodeSelector != nil || len(pl.Queue) > 0 || pl.Priority != nil || pl.Affinity != nil {
		app.Spec.Placement = pl
	}

	// readiness
	if rd := comp.Root.Readiness; rd != nil {
		app.Spec.Readiness = &Readiness{}
		switch {
		case len(rd.TCP) > 0:
			p, _ := toInt(portOf(km, rd.TCP))
			app.Spec.Readiness.TCP = &TCPProbe{Port: p}
		case len(rd.HTTP) > 0:
			pn, pth, _ := strings.Cut(rd.HTTP, "/")
			p, _ := toInt(portOf(km, pn))
			app.Spec.Readiness.HTTP = &HTTPProbe{Port: p, Path: "/" + pth}
		case len(rd.Exec) > 0:
			app.Spec.Readiness.Exec = rd.Exec
		}
	}

	// volumes: claims, libraries, config secrets
	if cm, ok := km["claims"].(map[string]any); ok {
		for _, name := range sortedKeys(cm) {
			c, _ := cm[name].(map[string]any)
			app.Spec.Volumes = append(app.Spec.Volumes, VolumeMount{MountPath: str(c["mountPath"]),
				Volume: VolumeSource{PV: &PVVolume{Size: str(c["size"])}}})
		}
	}
	for _, lib := range libs {
		lc := r.b.Components[lib]
		mount := comp.Schema.Root.Properties[libKey(comp, lib)].Mount
		if len(lc.ImageRef) == 0 {
			return nil, nil, fmt.Errorf("library %s has no image reference", lib)
		}
		app.Spec.Volumes = append(app.Spec.Volumes, VolumeMount{MountPath: mount, Merge: true, Volume: VolumeSource{Image: &ImageVolume{Reference: lc.ImageRef}}})
	}
	secrets, mounts, err := r.configSecrets(key, comp, km, files)
	if err != nil {
		return nil, nil, err
	}
	app.Spec.Volumes = append(app.Spec.Volumes, mounts...)
	app.Metadata = &ChildMeta{Labels: map[string]string{LabelComponent: key}}
	return app, secrets, nil
}

// rendered is one file's bytes, per replica when the file is per-instance.
type rendered struct {
	dir, base string
	mode      *int32
	bytes     map[string][]byte // secret key → bytes: base, or base.<i>
	perInst   bool
	secrets   []string
}

// configSecrets renders every file of the component and groups them by
// directory into content-addressed Secrets, with the mounts that use them. A
// file of format secret is not rendered: the secret the leaf references is
// mounted at the path, one mount per (directory, secret).
func (r *render) configSecrets(key string, comp *bundle.Component, km map[string]any, files []file) ([]Secret, []VolumeMount, error) {
	var all []rendered
	secretMounts, err := r.secretFiles(key, km, files)
	if err != nil {
		return nil, nil, err
	}
	for _, f := range files {
		if f.node.File.Format == packschema.FormatSecret {
			continue
		}
		rel := strings.TrimPrefix(f.ptr, "/"+key)
		rd := rendered{dir: path.Dir(f.node.File.Path), base: path.Base(f.node.File.Path), bytes: map[string][]byte{}, perInst: f.node.File.PerInstance}
		if len(f.node.File.Mode) > 0 {
			m, err := strconv.ParseInt(f.node.File.Mode, 8, 32)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: mode %q: %w", f.ptr, f.node.File.Mode, err)
			}
			m32 := int32(m)
			rd.mode = &m32
		}
		templates := comp.Templates
		if len(f.lib) > 0 {
			templates = r.b.Components[f.lib].Templates
		}
		renderOne := func(doc map[string]any, instance map[string]any, secretKey string) error {
			sub, ok := get(doc, rel)
			if !ok || sub == nil {
				return nil // a section nobody filled is a section that does not exist
			}
			sub = schema.DeepCopy(sub)
			refs := placeholders(sub)
			rd.secrets = union(rd.secrets, refs)
			var b []byte
			var err error
			if f.node.File.Format == "template" {
				tmpl, ok := templates[f.node.File.Template]
				if !ok {
					return fmt.Errorf("%s: template %q not found", f.ptr, f.node.File.Template)
				}
				data := map[string]any{"values": doc, "horchestra": r.horchestra(key, instance)}
				if m, ok := sub.(map[string]any); ok {
					maps.Copy(data, m)
				} else {
					data["value"] = sub
				}
				b, err = emit.Template(f.node.File.Template, tmpl, data, f.node.File.ParseAs)
			} else {
				b, err = emit.Emit(f.node.File.Format, sub)
			}
			if err != nil {
				return fmt.Errorf("%s: %w", f.ptr, err)
			}
			rd.bytes[secretKey] = b
			return nil
		}
		if rd.perInst {
			n := max(r.replicas(key), 1)
			for i := range n {
				doc, err := r.instanceDoc(key, i)
				if err != nil {
					return nil, nil, err
				}
				inst := map[string]any{"instance": fmt.Sprintf("%s-%d", r.service(key), i), "index": i, "ordinal": i + 1}
				if err := renderOne(doc, inst, fmt.Sprintf("%s.%d", rd.base, i)); err != nil {
					return nil, nil, err
				}
			}
		} else if err := renderOne(km, nil, rd.base); err != nil {
			return nil, nil, err
		}
		if len(rd.bytes) > 0 {
			all = append(all, rd)
		}
	}
	// group by directory
	byDir := map[string][]rendered{}
	for _, rd := range all {
		byDir[rd.dir] = append(byDir[rd.dir], rd)
	}
	var secrets []Secret
	var mounts []VolumeMount
	for _, dir := range sortedKeysR(byDir) {
		group := byDir[dir]
		sort.Slice(group, func(i, j int) bool { return group[i].base < group[j].base })
		for i := 1; i < len(group); i++ {
			if group[i].base == group[i-1].base {
				return nil, nil, fmt.Errorf("%s/%s is written twice (a component and a library, or two files at one path)", dir, group[i].base)
			}
		}
		data := map[string]string{}
		binary := map[string]string{}
		var items []KeyToPath
		var subst []string
		var names []string
		for _, rd := range group {
			for _, k := range sortedKeysB(rd.bytes) {
				b := rd.bytes[k]
				if utf8.Valid(b) {
					data[k] = string(b)
				} else {
					binary[k] = base64.StdEncoding.EncodeToString(b)
				}
				names = append(names, k)
			}
			itemKey := rd.base
			if rd.perInst {
				itemKey = rd.base + ".${horchestra.index}"
			}
			items = append(items, KeyToPath{Key: itemKey, Path: rd.base, Mode: rd.mode})
			subst = union(subst, rd.secrets)
		}
		// A stable name — the installation, the key and the directory — so rendering again
		// updates the same object in place and the output is idempotent. What turns the update
		// into a restart is horchestra's: the set stamps the Secret's resourceVersion on the
		// children that mount it (horchestra.io/config-revision), and the node restarts on it.
		name := configSecretName(r.ctx.Installation, key, dir)
		secrets = append(secrets, Secret{
			APIVersion: "horchestra.io/v1", Kind: "Secret",
			Metadata: ObjectMeta{Name: name, Namespace: r.ctx.Namespace,
				Labels: map[string]string{LabelInstallation: r.ctx.Installation, LabelComponent: key}},
			Type: SecretTypeConfig, StringData: nilIfEmpty(data), Data: nilIfEmpty(binary),
		})
		sort.Strings(subst)
		mounts = append(mounts, VolumeMount{MountPath: dir, Merge: true, Volume: VolumeSource{Secret: &SecretVolume{Name: name, Items: items, Substitute: subst}}})
		r.report.Secrets[name] = names
	}
	return secrets, append(mounts, secretMounts...), nil
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

// secretFiles turns every x-pack-file of format secret into a mount of the
// referenced secret's key at the path — bytes untouched, so a keytab or a
// keystore arrives as it was stored. Per-instance: the key is <key>.<index>,
// resolved by the set controller. Files in one directory from one secret
// share a mount.
func (r *render) secretFiles(key string, km map[string]any, files []file) ([]VolumeMount, error) {
	type slot struct {
		dir, secret string
	}
	items := map[slot][]KeyToPath{}
	var order []slot
	for _, f := range files {
		if f.node.File.Format != packschema.FormatSecret {
			continue
		}
		rel := strings.TrimPrefix(f.ptr, "/"+key)
		val, ok := get(km, rel)
		if !ok || val == nil {
			continue // not provided: nothing to mount (the schema's required decides whether that is an error)
		}
		m, _ := val.(map[string]any)
		sr, _ := m["secretRef"].(map[string]any)
		if sr == nil {
			return nil, fmt.Errorf("%s: a secret file needs {secretRef: {name, key}}", f.ptr)
		}
		name, k := str(sr["name"]), str(sr["key"])
		if len(name) == 0 || len(k) == 0 {
			return nil, fmt.Errorf("%s: secretRef needs both name and key", f.ptr)
		}
		if f.node.File.PerInstance {
			k += ".${horchestra.index}"
		}
		item := KeyToPath{Key: k, Path: path.Base(f.node.File.Path)}
		if len(f.node.File.Mode) > 0 {
			mode, err := strconv.ParseInt(f.node.File.Mode, 8, 32)
			if err != nil {
				return nil, fmt.Errorf("%s: mode %q: %w", f.ptr, f.node.File.Mode, err)
			}
			m32 := int32(mode)
			item.Mode = &m32
		}
		s := slot{path.Dir(f.node.File.Path), name}
		if _, seen := items[s]; !seen {
			order = append(order, s)
		}
		items[s] = append(items[s], item)
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
	return mounts, nil
}

// placeholders replaces every {secretRef: {name, key}} in a subtree by the
// placeholder the node resolves, and returns the secret names it saw.
func placeholders(v any) []string {
	var names []string
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			if sr, ok := x["secretRef"].(map[string]any); ok && len(x) == 1 {
				name, key := str(sr["name"]), str(sr["key"])
				names = union(names, []string{name})
				return fmt.Sprintf("${secret:%s/%s}", name, key)
			}
			for k, e := range x {
				x[k] = walk(e)
			}
		case []any:
			for i, e := range x {
				x[i] = walk(e)
			}
		}
		return v
	}
	walk(v)
	return names
}

// argv renders one annotated leaf into argv elements.
func argv(a *packschema.Arg, val any, present bool) ([]string, error) {
	if !present || val == nil {
		if a.OmitEmpty {
			return nil, nil
		}
		return nil, fmt.Errorf("x-pack-arg %s: no value and not omitEmpty", a.Flag)
	}
	if s, ok := val.(string); ok && len(s) == 0 && a.OmitEmpty {
		return nil, nil
	}
	var values []string
	if list, ok := val.([]any); ok && a.Each {
		for _, e := range list {
			s, err := scalarString(e)
			if err != nil {
				return nil, err
			}
			values = append(values, s)
		}
	} else {
		s, err := scalarString(val)
		if err != nil {
			return nil, err
		}
		values = []string{s}
	}
	var out []string
	for _, v := range values {
		v = a.Prefix + v
		if strings.HasSuffix(a.Flag, "=") || strings.HasSuffix(a.Flag, ":") {
			out = append(out, a.Flag+v)
		} else {
			out = append(out, a.Flag, v)
		}
	}
	return out, nil
}

func envVar(name string, val any) (EnvVar, error) {
	if m, ok := val.(map[string]any); ok {
		if sr, ok := m["secretRef"].(map[string]any); ok {
			return EnvVar{Name: name, SecretRef: &SecretRef{Name: str(sr["name"]), Key: str(sr["key"])}}, nil
		}
		return EnvVar{}, fmt.Errorf("x-pack-env %s: the leaf is an object", name)
	}
	s, err := scalarString(val)
	if err != nil {
		return EnvVar{}, fmt.Errorf("x-pack-env %s: %w", name, err)
	}
	return EnvVar{Name: name, Value: s}, nil
}

func scalarString(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case json.Number:
		return x.String(), nil
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			s, err := scalarString(e)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ","), nil
	}
	return "", fmt.Errorf("not a scalar (%T)", v)
}

func portOf(km map[string]any, name string) any {
	if pm, ok := km["ports"].(map[string]any); ok {
		return pm[name]
	}
	return nil
}

func libKey(comp *bundle.Component, lib string) string {
	for name, n := range comp.Schema.Root.Properties {
		if n.IsForeign() && n.Ref == packschema.PackScheme+lib+"#" {
			return name
		}
	}
	return ""
}

func str(v any) string {
	s, _ := v.(string)
	return s
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
		out[k] = str(e)
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

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysR(m map[string][]rendered) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysB(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
