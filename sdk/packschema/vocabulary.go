package packschema

import (
	"encoding/json"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
)

// VocabularyID is the resource URL of the vocabulary's schema — the same
// document that carries $defs/secretRef for the validator.
const VocabularyID = "horchestra://pack/v1"

// Vocabulary renders the x-pack-* vocabulary as one JSON Schema document:
// $defs/root (x-pack on a component or composition root), key (x-pack on a
// composition key), file, arg, hook, readiness, peer, upgrade, when, step,
// rollout — reflected from the Go types the tool decodes them into — plus
// secretRef, what a password leaf becomes in the validating view. The tool
// ships it as a blob; an author on another language validates annotations
// against it.
func Vocabulary() ([]byte, error) {
	r := jsonschema.Reflector{Anonymous: true, DoNotReference: true, RequiredFromJSONSchemaTags: true}
	defs := map[string]*jsonschema.Schema{}
	for name, v := range map[string]any{
		"root": Root{}, "key": Key{}, "file": File{}, "arg": Arg{}, "hook": Hook{},
		"readiness": Readiness{}, "peer": Peer{}, "upgrade": Upgrade{}, "when": When{},
		"step": Step{}, "rollout": Rollout{}, "claim": Claim{},
	} {
		s := r.Reflect(v)
		s.Version = ""
		defs[name] = s
	}
	// secretRef: only a reference is ever accepted where a password lives.
	defs["secretRef"] = &jsonschema.Schema{
		Type: "object", Required: []string{"secretRef"},
		AdditionalProperties: jsonschema.FalseSchema,
		Properties: props(map[string]*jsonschema.Schema{
			"secretRef": {
				Type: "object", Required: []string{"name", "key"},
				AdditionalProperties: jsonschema.FalseSchema,
				Properties: props(map[string]*jsonschema.Schema{
					"name": {Type: "string", MinLength: new(uint64(1))},
					"key":  {Type: "string", MinLength: new(uint64(1))},
				}),
			},
		}),
	}
	doc := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     VocabularyID,
		"$defs":   defs,
	}

	return json.Marshal(doc)
}

func props(m map[string]*jsonschema.Schema) *orderedmap.OrderedMap[string, *jsonschema.Schema] {
	p := jsonschema.NewProperties()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		p.Set(k, m[k])
	}
	return p
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
