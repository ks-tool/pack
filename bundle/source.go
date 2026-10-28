package bundle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ks-tool/pack/registry"
	"github.com/ks-tool/pack/sdk"
)

// The artifact types and annotations of a pack's members are sdk's.
const (
	ArtifactPack            = sdk.ArtifactPack
	ArtifactPackSchema      = sdk.ArtifactPackSchema
	ArtifactComponent       = sdk.ArtifactComponent
	ArtifactComponentSchema = sdk.ArtifactComponentSchema
	ArtifactTemplate        = sdk.ArtifactTemplate
	AnnotationName          = sdk.AnnotationName
	AnnotationVersion       = sdk.AnnotationVersion
)

// Files is what one artifact — a composition or a component — contributes to
// a bundle, wherever it came from: a directory on disk or a registry.
type Files struct {
	Name, Version string
	Origin        string // the directory, or the registry reference — for messages
	Schema        []byte
	Templates     map[string][]byte
	ImageRef      string // the image the component runs, "" for a library or a composition
	ImageConfig   []byte // the image's OCI config, optional
}

// Source hands out the files of the artifact a reference names and of the
// components it refers to by name.
type Source interface {
	Root() (*Files, error)
	Member(name string) (*Files, error)
}

// ---- a directory tree: the author's working tree, or oci-packer extract's output

type dirSource struct{ dir string }

// Dir is the source rooted at a composition or component directory; members
// are the sibling directories.
// MaxTemplate bounds one template of a component read from a registry: a raw
// file is the component's data, byte for byte.
const MaxTemplate = 1 << 30

func Dir(dir string) Source { return dirSource{dir} }

func (d dirSource) Root() (*Files, error) { return dirFiles(d.dir) }

func (d dirSource) Member(name string) (*Files, error) {
	return dirFiles(filepath.Join(filepath.Dir(d.dir), name))
}

func dirFiles(dir string) (*Files, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "schema.json"))
	if err != nil {
		return nil, err
	}
	f := &Files{Origin: dir, Schema: raw, Templates: map[string][]byte{}}
	entries, _ := os.ReadDir(filepath.Join(dir, "templates"))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if f.Templates[e.Name()], err = os.ReadFile(filepath.Join(dir, "templates", e.Name())); err != nil {
			return nil, err
		}
	}
	if cfg, err := os.ReadFile(filepath.Join(dir, "image.json")); err == nil {
		f.ImageConfig = cfg
	}
	f.Name, f.Version = packFileIdentity(dir)
	if len(f.Name) == 0 {
		f.Name = filepath.Base(dir)
	}
	f.ImageRef = imageRef(dir)
	return f, nil
}

// ---- a registry: the pack as oci-packer pushed it — read, never pulled

type registrySource struct {
	ctx     context.Context
	c       *registry.Client
	ref     registry.Ref
	root    *Files
	members map[string]ocispec.Descriptor // io.horchestra.name → member index, once listed
	// read is every member's files, fetched together the first time any member is asked for:
	// a pack's members are independent artifacts, and reading them one after another made the
	// registry round trips the whole cost of a render (27 members: 236 requests, 20 s on a LAN).
	read map[string]memberFiles
}

type memberFiles struct {
	files *Files
	err   error
}

// registryWorkers bounds how many members are read at once — enough to hide the round-trip
// latency, few enough that a registry's rate limit is not the next error.
const registryWorkers = 8

// Registry is the source rooted at a registry reference: the index the
// reference names, and the artifacts mounted into it. Only manifests, the
// schemas, the templates and image configs are read — no layer, ever.
func Registry(ctx context.Context, c *registry.Client, ref registry.Ref) Source {
	return &registrySource{ctx: ctx, c: c, ref: ref}
}

func (r *registrySource) Root() (*Files, error) {
	if r.root != nil {
		return r.root, nil
	}
	mt, dgst, body, err := r.c.Manifest(r.ctx, r.ref.Host, r.ref.Repo, r.ref.Reference())
	if err != nil {
		return nil, err
	}
	if !isIndex(mt) {
		return nil, fmt.Errorf("%s: not an index (%s) — a pack or a component is an OCI index", r.ref, mt)
	}
	var idx ocispec.Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("%s: %w", r.ref, err)
	}
	f, err := r.files(idx, dgst)
	if err != nil {
		return nil, err
	}
	r.root = f
	return f, nil
}

func (r *registrySource) Member(name string) (*Files, error) {
	if r.root != nil && r.root.Name == name && r.root.Schema != nil && len(r.root.ImageRef) > 0 {
		return r.root, nil // a lone component asked for itself
	}
	if r.members == nil {
		if err := r.list(); err != nil {
			return nil, err
		}
	}
	m, ok := r.read[name]
	if !ok {
		if len(r.members) == 0 {
			return nil, fmt.Errorf("%s: nothing named %q is mounted into it — a component's libraries are members of the pack, or mounted into the component itself when it is to be rendered alone", r.ref, name)
		}
		names := make([]string, 0, len(r.members))
		for n := range r.members {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("%s: no member named %q (members: %s)", r.ref, name, strings.Join(names, ", "))
	}
	return m.files, m.err
}

// parallel runs fn over n items with at most registryWorkers in flight and waits for all of
// them. Each item owns its slot in whatever fn writes to, so the result is the same whatever
// order the registry answered in.
func parallel(n int, fn func(i int)) {
	sem := make(chan struct{}, registryWorkers)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}()
	}
	wg.Wait()
}

// list reads every index mounted into the root and remembers it by name: a
// member's descriptor carries no annotations, the mounted index does.
func (r *registrySource) list() error {
	if _, err := r.Root(); err != nil {
		return err
	}
	_, _, body, err := r.c.Manifest(r.ctx, r.ref.Host, r.ref.Repo, r.ref.Reference())
	if err != nil {
		return err
	}
	var idx ocispec.Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return err
	}
	// Every member's index at once, then every member's files at once: the two rounds are all
	// the registry sees of a render, and neither depends on the order the answers arrive in.
	var subs []ocispec.Descriptor
	for _, m := range idx.Manifests {
		if isIndex(m.MediaType) {
			subs = append(subs, m)
		}
	}
	indexes := make([]ocispec.Index, len(subs))
	errs := make([]error, len(subs))
	parallel(len(subs), func(i int) { indexes[i], errs[i] = r.index(subs[i].Digest.String()) })
	r.members = map[string]ocispec.Descriptor{}
	var named []int
	for i, m := range subs {
		if errs[i] != nil {
			return errs[i]
		}
		name := indexes[i].Annotations[AnnotationName]
		if len(name) == 0 {
			name = m.Annotations[AnnotationName]
		}
		if len(name) == 0 {
			continue
		}
		if _, dup := r.members[name]; dup {
			return fmt.Errorf("%s: two members named %q — one version of a component per pack", r.ref, name)
		}
		r.members[name] = m
		named = append(named, i)
	}
	read := make([]memberFiles, len(named))
	parallel(len(named), func(j int) {
		i := named[j]
		f, err := r.files(indexes[i], subs[i].Digest.String())
		read[j] = memberFiles{files: f, err: err}
	})
	r.read = map[string]memberFiles{}
	for j, i := range named {
		r.read[indexes[i].Annotations[AnnotationName]] = read[j]
	}
	for name, m := range r.members {
		if _, ok := r.read[name]; !ok { // named by the member descriptor, not the nested index
			for j, i := range named {
				if subs[i].Digest == m.Digest {
					r.read[name] = read[j]
				}
			}
		}
	}
	return nil
}

func (r *registrySource) index(digest string) (ocispec.Index, error) {
	var idx ocispec.Index
	_, _, body, err := r.c.Manifest(r.ctx, r.ref.Host, r.ref.Repo, digest)
	if err != nil {
		return idx, err
	}
	if err := json.Unmarshal(body, &idx); err != nil {
		return idx, fmt.Errorf("%s@%s: %w", r.ref.Repo, digest, err)
	}
	return idx, nil
}

func (r *registrySource) manifest(digest string) (ocispec.Manifest, error) {
	var m ocispec.Manifest
	_, _, body, err := r.c.Manifest(r.ctx, r.ref.Host, r.ref.Repo, digest)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, fmt.Errorf("%s@%s: %w", r.ref.Repo, digest, err)
	}
	return m, nil
}

// files reads one artifact's index: the schema member, the template members
// (each layer at its title), and the image member — its reference in the
// pack's repository, and its config, never its layers.
func (r *registrySource) files(idx ocispec.Index, digest string) (*Files, error) {
	f := &Files{
		Origin:    r.ref.Host + "/" + r.ref.Repo + "@" + digest,
		Name:      idx.Annotations[AnnotationName],
		Version:   idx.Annotations[AnnotationVersion],
		Templates: map[string][]byte{},
	}
	for _, m := range idx.Manifests {
		switch {
		case m.ArtifactType == ArtifactComponentSchema || m.ArtifactType == ArtifactPackSchema:
			b, err := r.firstLayer(m)
			if err != nil {
				return nil, fmt.Errorf("%s: schema: %w", f.Origin, err)
			}
			f.Schema = b
		case m.ArtifactType == ArtifactTemplate:
			man, err := r.manifest(m.Digest.String())
			if err != nil {
				return nil, err
			}
			for _, l := range man.Layers {
				title := l.Annotations[ocispec.AnnotationTitle]
				if len(title) == 0 {
					return nil, fmt.Errorf("%s: a template layer without a title", f.Origin)
				}
				title = strings.TrimPrefix(title, "templates/")
				// a template is the component's file: a script, or a raw file — a dump,
				// an archive of data — as large as the layer says, up to MaxTemplate
				if l.Size > MaxTemplate {
					return nil, fmt.Errorf("%s: template %s: %d bytes, more than %d", f.Origin, title, l.Size, int64(MaxTemplate))
				}
				b, err := r.c.Blob(r.ctx, r.ref.Host, r.ref.Repo, l.Digest.String(), max(l.Size, 1<<20))
				if err != nil {
					return nil, fmt.Errorf("%s: template %s: %w", f.Origin, title, err)
				}
				f.Templates[title] = b
			}
		case m.Platform != nil || isImageManifest(m.MediaType) && len(m.ArtifactType) == 0:
			if len(f.ImageRef) > 0 {
				continue // one image per platform: the first one names it, the reference is the same digest anyway
			}
			f.ImageRef = r.ref.Host + "/" + r.ref.Repo + "@" + m.Digest.String()
			man, err := r.manifest(m.Digest.String())
			if err != nil {
				return nil, err
			}
			if len(man.Config.Digest) > 0 && man.Config.Size < 4<<20 {
				cfg, err := r.c.Blob(r.ctx, r.ref.Host, r.ref.Repo, man.Config.Digest.String(), 4<<20)
				if err != nil {
					return nil, fmt.Errorf("%s: image config: %w", f.Origin, err)
				}
				f.ImageConfig = cfg
			}
		}
	}
	if f.Schema == nil {
		return nil, fmt.Errorf("%s: no schema member (%s or %s)", f.Origin, ArtifactComponentSchema, ArtifactPackSchema)
	}
	return f, nil
}

func (r *registrySource) firstLayer(d ocispec.Descriptor) ([]byte, error) {
	man, err := r.manifest(d.Digest.String())
	if err != nil {
		return nil, err
	}
	if len(man.Layers) == 0 {
		return nil, fmt.Errorf("%s: no layers", d.Digest)
	}
	return r.c.Blob(r.ctx, r.ref.Host, r.ref.Repo, man.Layers[0].Digest.String(), 4<<20)
}

func isIndex(mt string) bool {
	return mt == ocispec.MediaTypeImageIndex || mt == "application/vnd.docker.distribution.manifest.list.v2+json"
}

func isImageManifest(mt string) bool {
	return mt == ocispec.MediaTypeImageManifest || mt == "application/vnd.docker.distribution.manifest.v2+json"
}
