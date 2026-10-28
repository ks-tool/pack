package sdk

import (
	"strings"
	"testing"
)

type leaf struct{ name string }

func (l leaf) Meta() Meta {
	return Meta{Name: l.name, Version: "1.0.0", Image: "oci://images/" + l.name + ":1.0.0", Artifact: "oci://packs/" + l.name}
}

type group struct {
	name    string
	members []Component
}

func (g group) Meta() Meta {
	return Meta{Name: g.name, Version: "2.0.0", Artifact: "oci://packs/" + g.name}
}
func (g group) Members() []Component { return g.members }

// The pack file of a composition lists every member it holds — the members of
// a nested composition too, after it, each once.
func TestPackFile_CompositionListsEveryMemberOnce(t *testing.T) {
	zk := leaf{"zookeeper"}
	mid := group{"mid", []Component{leaf{"a"}, zk}}
	top := group{"top", []Component{zk, mid, leaf{"b"}}}
	raw, err := PackFileYAML(top)
	if err != nil {
		t.Fatal(err)
	}
	var from []string
	for _, l := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "- ")), "from: "); ok {
			from = append(from, v)
		}
	}
	want := []string{"file://schema.json", "oci://packs/zookeeper:1.0.0", "oci://packs/mid:2.0.0", "oci://packs/a:1.0.0", "oci://packs/b:1.0.0"}
	if strings.Join(from, " ") != strings.Join(want, " ") {
		t.Errorf("items:\n got %v\nwant %v\n%s", from, want, raw)
	}
}

func TestPackFile_NestedMemberNeedsAnArtifact(t *testing.T) {
	bad := group{"mid", []Component{noArtifact{}}}
	if _, err := PackFile(group{"top", []Component{bad}}); err == nil || !strings.Contains(err.Error(), "no Artifact") {
		t.Errorf("a nested member without an Artifact: %v", err)
	}
}

type noArtifact struct{}

func (noArtifact) Meta() Meta { return Meta{Name: "x", Version: "1"} }
