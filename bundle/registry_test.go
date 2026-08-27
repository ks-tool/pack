package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ks-tool/pack/registry"
)

// fakeRegistry serves what oci-packer pushes: per repository, manifests by
// tag or digest and blobs by digest. It is built from the example
// directories the way the pack files describe them.
type fakeRegistry struct {
	mu    sync.Mutex
	blobs map[string][]byte // digest → bytes, shared by every repo (mounts)
	tags  map[string]string // repo:tag → digest
	types map[string]string // digest → media type
	hits  map[string]int    // path prefix → requests, to prove no layer is read
}

func newFake() *fakeRegistry {
	return &fakeRegistry{blobs: map[string][]byte{}, tags: map[string]string{}, types: map[string]string{}, hits: map[string]int{}}
}

func (f *fakeRegistry) put(b []byte, mediaType string) ocispec.Descriptor {
	sum := sha256.Sum256(b)
	d := "sha256:" + hex.EncodeToString(sum[:])
	f.blobs[d] = b
	f.types[d] = mediaType
	return ocispec.Descriptor{MediaType: mediaType, Digest: digest.Digest(d), Size: int64(len(b))}
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v2/")
	var kind, repo, ref string
	if i := strings.LastIndex(p, "/manifests/"); i >= 0 {
		kind, repo, ref = "manifests", p[:i], p[i+len("/manifests/"):]
	} else if i := strings.LastIndex(p, "/blobs/"); i >= 0 {
		kind, repo, ref = "blobs", p[:i], p[i+len("/blobs/"):]
	} else {
		http.NotFound(w, r)
		return
	}
	f.hits[kind]++
	if kind == "blobs" {
		f.hits["layer:"+ref]++
	}
	d := ref
	if !strings.HasPrefix(ref, "sha256:") {
		d = f.tags[repo+":"+ref]
	}
	b, ok := f.blobs[d]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if kind == "manifests" {
		w.Header().Set("Content-Type", f.types[d])
	}
	w.Header().Set("Docker-Content-Digest", d)
	w.Write(b)
}

// component packs a directory the way `oci-packer -f pack.yaml` does: an
// index with the image manifest (a fake one with the directory's image.json
// as config, layers that are not there to be read), the schema manifest, the
// template manifest with a layer per file.
func (f *fakeRegistry) component(t *testing.T, dir, name, version string, libs ...ocispec.Descriptor) ocispec.Descriptor {
	t.Helper()
	schemaRaw, err := os.ReadFile(filepath.Join(dir, "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var members []ocispec.Descriptor
	// the image: config from image.json (or a minimal one), layers as opaque blobs
	cfg, err := os.ReadFile(filepath.Join(dir, "image.json"))
	if err != nil {
		cfg = []byte(`{"architecture":"amd64","os":"linux","config":{"Entrypoint":["/bin/app"]}}`)
	}
	cfgDesc := f.put(cfg, ocispec.MediaTypeImageConfig)
	layer := f.put([]byte("layer bytes of "+name), ocispec.MediaTypeImageLayerGzip)
	img, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: cfgDesc, Layers: []ocispec.Descriptor{layer}})
	imgDesc := f.put(img, ocispec.MediaTypeImageManifest)
	imgDesc.Platform = &ocispec.Platform{Architecture: "amd64", OS: "linux"}
	members = append(members, imgDesc)
	// the schema
	sl := f.put(schemaRaw, "application/json")
	sl.Annotations = map[string]string{ocispec.AnnotationTitle: "schema.json"}
	sm, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, ArtifactType: ArtifactComponentSchema,
		Config: ocispec.DescriptorEmptyJSON, Layers: []ocispec.Descriptor{sl}})
	sd := f.put(sm, ocispec.MediaTypeImageManifest)
	sd.ArtifactType = ArtifactComponentSchema
	members = append(members, sd)
	// the templates
	entries, _ := os.ReadDir(filepath.Join(dir, "templates"))
	if len(entries) > 0 {
		var layers []ocispec.Descriptor
		for _, e := range entries {
			b, _ := os.ReadFile(filepath.Join(dir, "templates", e.Name()))
			l := f.put(b, "application/octet-stream")
			l.Annotations = map[string]string{ocispec.AnnotationTitle: e.Name(), "io.arenadata.oci-packer.dir": "templates/"}
			layers = append(layers, l)
		}
		tm, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, ArtifactType: ArtifactTemplate, Config: ocispec.DescriptorEmptyJSON, Layers: layers})
		td := f.put(tm, ocispec.MediaTypeImageManifest)
		td.ArtifactType = ArtifactTemplate
		members = append(members, td)
	}
	for _, l := range libs { // a lone component may mount the libraries it needs
		l.Annotations = nil
		members = append(members, l)
	}
	idx, _ := json.Marshal(ocispec.Index{MediaType: ocispec.MediaTypeImageIndex, ArtifactType: ArtifactComponent, Manifests: members,
		Annotations: map[string]string{AnnotationName: name, AnnotationVersion: version}})
	d := f.put(idx, ocispec.MediaTypeImageIndex)
	f.tags["components/"+name+":"+version] = d.Digest.String()
	return d
}

func (f *fakeRegistry) pack(t *testing.T, dir, name, version string, components []ocispec.Descriptor) {
	t.Helper()
	schemaRaw, err := os.ReadFile(filepath.Join(dir, "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	sl := f.put(schemaRaw, "application/json")
	sm, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, ArtifactType: ArtifactPackSchema, Config: ocispec.DescriptorEmptyJSON, Layers: []ocispec.Descriptor{sl}})
	sd := f.put(sm, ocispec.MediaTypeImageManifest)
	sd.ArtifactType = ArtifactPackSchema
	members := append([]ocispec.Descriptor{sd}, components...)
	for i := range members[1:] {
		members[i+1].Annotations = nil // a mounted member's descriptor carries no annotations; the index does
	}
	idx, _ := json.Marshal(ocispec.Index{MediaType: ocispec.MediaTypeImageIndex, ArtifactType: ArtifactPack, Manifests: members,
		Annotations: map[string]string{AnnotationName: name, AnnotationVersion: version}})
	d := f.put(idx, ocispec.MediaTypeImageIndex)
	f.tags["packs/"+name+":"+version] = d.Digest.String()
}

// The examples, pushed to a fake registry, load into the same bundle as the
// directories do — same schemas, templates and configs, the image by digest
// in the pack's repository — and not one layer is fetched.
func TestLoadRef_MatchesDirectories(t *testing.T) {
	ex := filepath.Join("..", "examples")
	f := newFake()
	jdk := f.component(t, filepath.Join(ex, "jdk"), "jdk", "21.0.4")
	members := []ocispec.Descriptor{jdk,
		f.component(t, filepath.Join(ex, "zookeeper"), "zookeeper", "3.9.2", jdk), // mounts its library: installable alone
		f.component(t, filepath.Join(ex, "kafka"), "kafka", "3.7.1"),
		f.component(t, filepath.Join(ex, "postgres"), "postgres", "16.3"),
	}
	f.pack(t, filepath.Join(ex, "adh"), "adh", "2.1.0", members)
	srv := httptest.NewServer(f)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	c := &registry.Client{PlainHTTP: true}
	got, err := LoadRef(context.Background(), c, "cr://"+host+"/packs/adh:2.1.0")
	if err != nil {
		t.Fatal(err)
	}
	want, err := Load(filepath.Join(ex, "adh"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != want.Name || got.Version != want.Version || strings.Join(got.Keys, ",") != strings.Join(want.Keys, ",") {
		t.Errorf("identity/keys: got %s %s %v, want %s %s %v", got.Name, got.Version, got.Keys, want.Name, want.Version, want.Keys)
	}
	for name, w := range want.Components {
		g, ok := got.Components[name]
		if !ok {
			t.Errorf("component %s missing", name)
			continue
		}
		if string(g.SchemaRaw) != string(w.SchemaRaw) || g.Version != w.Version || g.IsLibrary != w.IsLibrary {
			t.Errorf("%s: schema/version/library differ", name)
		}
		if len(g.Templates) != len(w.Templates) {
			t.Errorf("%s: templates %d vs %d", name, len(g.Templates), len(w.Templates))
		}
		for tn, tb := range w.Templates {
			if string(g.Templates[tn]) != string(tb) {
				t.Errorf("%s: template %s differs", name, tn)
			}
		}
		if !strings.HasPrefix(g.ImageRef, host+"/packs/adh@sha256:") {
			t.Errorf("%s: image must be a digest in the pack's repository, got %q", name, g.ImageRef)
		}
		if g.Image == nil || len(g.Image.Config.Entrypoint) == 0 {
			t.Errorf("%s: the image config (entrypoint) comes from the registry, got %+v", name, g.Image)
		}
	}
	// what was read: manifests and small blobs; the layer blobs never
	for d, b := range f.blobs {
		if strings.HasPrefix(string(b), "layer bytes of ") {
			if f.hits["layer:"+d] > 0 {
				t.Errorf("layer %s was fetched", d)
			}
		}
	}

	// a lone component by reference: its library is mounted into it
	one, err := LoadRef(context.Background(), c, host+"/components/zookeeper:3.9.2")
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Keys) != 1 || one.Keys[0] != "zookeeper" || one.Components["jdk"] == nil {
		t.Errorf("lone component: keys %v, jdk %v", one.Keys, one.Components["jdk"] != nil)
	}
	// kafka mounts nothing: its library is out of reach by reference
	if _, err := LoadRef(context.Background(), c, host+"/components/kafka:3.7.1"); err == nil || !strings.Contains(err.Error(), "jdk") {
		t.Errorf("a lone component without its library must fail naming it, got %v", err)
	}
}
