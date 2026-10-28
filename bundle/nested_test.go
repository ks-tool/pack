package bundle

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// tree writes name → schema.json under a fresh directory; a component also gets
// the image its pack file names.
func tree(t *testing.T, docs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, doc := range docs {
		d := filepath.Join(dir, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "schema.json"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(doc, `"platform"`) {
			pf := "type: application/vnd.horchestra.component.v1+json\nannotations:\n  io.horchestra.name: " + name +
				"\n  io.horchestra.version: \"1.0.0\"\nitems:\n  - from: oci://images/" + name + ":1.0.0\n"
			if err := os.WriteFile(filepath.Join(d, "pack.yaml"), []byte(pf), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

func component(extra string) string {
	return `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,
	  "x-pack":{"scale":"single"},"properties":{"platform":{"$ref":"urn:pack:platform","default":{}}` + extra + `}}`
}

func comp(props, xpack string) string {
	if xpack == "" {
		xpack = "{}"
	}
	return `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,
	  "x-pack":` + xpack + `,"properties":{` + props + `}}`
}

func ref(name string) string { return `{"$ref":"pack://` + name + `#"}` }

// stand names two compositions, which both hold zookeeper; mid is held by top
// under a key of its own, with `after`.
func stand(t *testing.T) *Bundle {
	t.Helper()
	dir := tree(t, map[string]string{
		"zookeeper": component(""), "a": component(""), "b": component(""), "solo": component(""),
		"mid": comp(`"common":{"type":"object","properties":{"realm":{"type":"string"}}},
		  "zookeeper":`+ref("zookeeper")+`,"a":{"$ref":"pack://a#","x-pack":{"after":["zookeeper"]}},"b":`+ref("b"),
			`{"savepoints":["a"],"hooks":[{"name":"init","phase":"postInstall","image":"a","command":["x"],"for":["a"]}],"rollout":{"maxUnavailable":2}}`),
		"top": comp(`"common":{"type":"object","properties":{"zone":{"type":"string"}}},
		  "solo":`+ref("solo")+`,
		  "adh":{"$ref":"pack://mid#","x-pack":{"after":["solo"]}},
		  "zookeeper":{"$ref":"pack://zookeeper#","x-pack":{"spread":true}}`,
			`{"savepoints":["solo"],"rollout":{"maxUnavailable":1}}`),
	})
	b, err := Load(filepath.Join(dir, "top"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The nested composition's keys take the place of the key that named it;
// the values stay flat.
func TestLoad_NestedCompositionIsFlattened(t *testing.T) {
	b := stand(t)
	if want := []string{"solo", "zookeeper", "a", "b"}; !slices.Equal(b.Keys, want) {
		t.Errorf("keys %v, want %v", b.Keys, want)
	}
	if _, ok := b.KeyComponent["adh"]; ok {
		t.Error("the key that named a composition is not a key")
	}
	for _, k := range b.Keys {
		if b.KeyComponent[k] != k || b.Components[k] == nil {
			t.Errorf("%s: %q %v", k, b.KeyComponent[k], b.Components[k])
		}
	}
}

// A key two compositions hold is one key; what the outer said of the key that
// named the nested composition is `after` of each of its keys.
func TestLoad_NestedKeysJoinAnnotations(t *testing.T) {
	b := stand(t)
	ann := func(k string) (after []string, spread bool) {
		a, err := b.Composition.Root.Properties[k].KeyAnnotation()
		if err != nil {
			t.Fatal(err)
		}
		return a.After, a.Spread
	}
	if after, spread := ann("zookeeper"); !spread || !slices.Equal(after, []string{"solo"}) {
		t.Errorf("zookeeper: spread by top, after solo by the key that named mid: %v %v", after, spread)
	}
	if after, _ := ann("a"); !slices.Equal(after, []string{"zookeeper", "solo"}) {
		t.Errorf("a keeps its own after and gains the outer's: %v", after)
	}
	if after, _ := ann("b"); !slices.Equal(after, []string{"solo"}) {
		t.Errorf("b: %v", after)
	}
}

// What the nested composition says of the system comes along.
func TestLoad_NestedSystemAnnotations(t *testing.T) {
	b := stand(t)
	if !slices.Equal(b.Root.Savepoints, []string{"solo", "a"}) {
		t.Errorf("savepoints %v", b.Root.Savepoints)
	}
	if len(b.Root.Hooks) != 1 || b.Root.Hooks[0].Name != "init" {
		t.Errorf("hooks %+v", b.Root.Hooks)
	}
	if b.Root.Rollout == nil || b.Root.Rollout.MaxUnavailable != 1 {
		t.Errorf("the outer's rollout stands: %+v", b.Root.Rollout)
	}
	common := b.Composition.Root.Properties["common"]
	if common == nil || common.Properties["realm"] == nil || common.Properties["zone"] == nil {
		t.Errorf("common holds both compositions' properties: %+v", common)
	}
}

func TestLoad_NestedCompositionRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		docs map[string]string
		want string
	}{
		"one key, two components": {map[string]string{
			"x": component(""), "y": component(""),
			"mid": comp(`"k":`+ref("x"), ""),
			"top": comp(`"mid":`+ref("mid")+`,"k":`+ref("y"), ""),
		}, `key "k" is x in one composition and y in another`},
		"a composition inside itself": {map[string]string{
			"x":   component(""),
			"mid": comp(`"x":`+ref("x")+`,"top":`+ref("top"), ""),
			"top": comp(`"mid":`+ref("mid"), ""),
		}, "contains itself"},
		"spread on a composition key": {map[string]string{
			"x":   component(""),
			"mid": comp(`"x":`+ref("x"), ""),
			"top": comp(`"mid":{"$ref":"pack://mid#","x-pack":{"spread":true}}`, ""),
		}, "only `after`"},
		"common defined two ways": {map[string]string{
			"x":   component(""),
			"mid": comp(`"common":{"type":"object","properties":{"realm":{"type":"string"}}},"x":`+ref("x"), ""),
			"top": comp(`"common":{"type":"object","properties":{"realm":{"type":"integer"}}},"mid":`+ref("mid"), ""),
		}, "common.realm is defined two ways"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := tree(t, tc.docs)
			_, err := Load(filepath.Join(dir, "top"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want …%s…, got %v", tc.want, err)
			}
		})
	}
}

// A composition without nested ones is read as before.
func TestLoad_PlainCompositionUnchanged(t *testing.T) {
	dir := tree(t, map[string]string{"x": component(""), "top": comp(`"x":`+ref("x"), "")})
	b, err := Load(filepath.Join(dir, "top"))
	if err != nil || !slices.Equal(b.Keys, []string{"x"}) {
		t.Fatalf("%v %v", b, err)
	}
}
