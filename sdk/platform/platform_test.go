package platform

import (
	"testing"

	"github.com/ks-tool/pack/sdk/packschema"
)

// pack_v1.json is what packschema.Vocabulary() generates — the same discipline
// as the examples' schema.json: `go generate ./platform/...`.
func TestVocabularyIsCurrent(t *testing.T) {
	want, err := packschema.Vocabulary()
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(PackV1) {
		t.Fatal("pack_v1.json is stale — run `go generate ./platform/...`")
	}
}
