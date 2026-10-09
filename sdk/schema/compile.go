package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/ks-tool/pack/sdk/platform"
)

// Compiled is the validating view of a schema: the same document with every
// `format: password` leaf swapped for the secretRef object, and its foreign
// $refs resolved to the documents the loader attached.
type Compiled struct {
	Validator *jsonschema.Schema
}

// Compile builds the validator with the platform slot open: any object.
func (s *Schema) Compile(id string) (*Compiled, error) { return s.CompileWith(id, nil) }

// CompileWith builds the validator, the platform slot the pack's half
// joined to a target's (platform.Merge); nil — the pack's half, open to
// properties it does not know. Foreign references are registered as
// resources under their own URLs, so the document itself is compiled
// unchanged apart from the password swap.
func (s *Schema) CompileWith(id string, targetPlatform []byte) (*Compiled, error) {
	slot, err := platform.Merge(targetPlatform)
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := addResource(c, platform.PackID, platform.PackV1); err != nil {
		return nil, err
	}
	if err := addResource(c, platform.SlotID, slot); err != nil {
		return nil, err
	}
	seen := map[string]bool{id: true} // the document itself is added last, under id
	if err := s.addForeign(c, seen); err != nil {
		return nil, err
	}
	view, err := json.Marshal(validationView(s.Raw))
	if err != nil {
		return nil, err
	}
	if err := addResource(c, id, view); err != nil {
		return nil, err
	}
	v, err := c.Compile(id)
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	return &Compiled{Validator: v}, nil
}

func (s *Schema) addForeign(c *jsonschema.Compiler, seen map[string]bool) error {
	for url, f := range s.Foreign {
		if url == platform.SlotID || url == platform.PackID || seen[url] {
			continue
		}
		seen[url] = true
		view, err := json.Marshal(validationView(f.Raw))
		if err != nil {
			return err
		}
		if err := addResource(c, url, view); err != nil {
			return err
		}
		if err := f.addForeign(c, seen); err != nil {
			return err
		}
	}
	return nil
}

func addResource(c *jsonschema.Compiler, url string, raw []byte) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	return c.AddResource(url, doc)
}

// validationView returns a deep copy where every {format: password} node is
// replaced by a $ref to the secretRef definition, keeping title/description.
func validationView(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if f, ok := x["format"].(string); ok && f == "password" {
			out := map[string]any{"$ref": platform.SecretRef}
			for _, keep := range []string{"title", "description"} {
				if t, ok := x[keep]; ok {
					out[keep] = t
				}
			}
			return out
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = validationView(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = validationView(e)
		}
		return out
	}
	return v
}

// Validate checks an instance and returns every violation as path + message.
func (c *Compiled) Validate(instance any) []Violation {
	err := c.Validator.Validate(instance)
	if err == nil {
		return nil
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []Violation{{Path: "", Message: err.Error()}}
	}
	var out []Violation
	flatten(ve, &out)
	if len(out) == 0 {
		out = append(out, Violation{Path: "/" + joinPath(ve.InstanceLocation), Message: ve.Error()})
	}
	return out
}

var printer = message.NewPrinter(language.English)

type Violation struct {
	Path    string
	Message string
}

func (v Violation) String() string { return v.Path + ": " + v.Message }

func flatten(ve *jsonschema.ValidationError, out *[]Violation) {
	if len(ve.Causes) == 0 {
		*out = append(*out, Violation{Path: "/" + joinPath(ve.InstanceLocation), Message: ve.ErrorKind.LocalizedString(printer)})
		return
	}
	for _, c := range ve.Causes {
		flatten(c, out)
	}
}

func joinPath(segs []string) string {
	var out strings.Builder
	for i, s := range segs {
		if i > 0 {
			out.WriteString("/")
		}
		out.WriteString(escape(s))
	}
	return out.String()
}
