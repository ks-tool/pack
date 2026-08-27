package packschema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/invopop/jsonschema"
)

// Rooter gives the root annotation of a component: x-pack.
type Rooter interface{ PackRoot() Root }

// Computer gives the formulas, keyed by JSON Pointer relative to the component
// root. CEL does not live in a struct tag: quotes and commas.
type Computer interface{ PackCompute() map[string]string }

// Reflect turns a Go default value into the component's schema. The VALUE is
// the defaults document: a field tagged pack:"const" becomes a fact (const),
// pack:"default" a setting with a default, an untagged field is the operator's.
//
// Tag grammar — comma-separated, key=value split on the FIRST '=':
//
//	const | default
//	ref=<uri>[,mount=<path>]                   the field's schema is foreign ($ref) — the platform half
//	pack_ref:"<name>"  (its own tag)            a member of the pack by name: a library (with pack:"mount=<path>")
//	                                           or a composition key (with pack:"default,spread,…")
//	file=<path>,format=<f>[,template=,parseAs=,mode=,perInstance]
//	arg=<flag>[,prefix=<p>][,each][,omitEmpty]
//	env=<NAME>
//	spread | peers=<role>:<key>;… | after=<key>;… | minimum=<res>:<qty>;…   (a composition key)
//
// A composition is reflected the same way: its fields are the keys of the
// system, each a ref= to a component, and PackRoot carries savepoints, hooks
// and rollout.
func Reflect(defaults any) (*jsonschema.Schema, error) {
	r := jsonschema.Reflector{
		Anonymous:                  true,
		ExpandedStruct:             true,
		DoNotReference:             true,
		RequiredFromJSONSchemaTags: true,
	}
	s := r.Reflect(defaults)
	s.Version = jsonschema.Version

	if root, ok := defaults.(Rooter); ok {
		extras(s)["x-pack"] = root.PackRoot()
	}
	if err := annotate(s, reflect.ValueOf(defaults), ""); err != nil {
		return nil, err
	}
	if c, ok := defaults.(Computer); ok {
		for ptr, expr := range c.PackCompute() {
			node, err := lookup(s, ptr)
			if err != nil {
				return nil, fmt.Errorf("x-pack-compute %s: %w", ptr, err)
			}
			extras(node)["x-pack-compute"] = expr
		}
	}
	return s, nil
}

func annotate(s *jsonschema.Schema, v reflect.Value, path string) error {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct || s.Properties == nil {
		return nil
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous && len(jsonTagName(f)) == 0 {
			// an embedded struct: its fields are promoted into this schema
			// (jsonschema inlines it), so its tags are annotated at this level
			if err := annotate(s, v.Field(i), path); err != nil {
				return err
			}
			continue
		}
		name := jsonName(f)
		if len(name) == 0 {
			continue
		}
		prop, ok := s.Properties.Get(name)
		if !ok {
			return fmt.Errorf("%s/%s: not in the reflected schema", path, name)
		}
		fv := v.Field(i)
		opts := parseTag(f.Tag.Get("pack"))
		if name := f.Tag.Get("pack_ref"); len(name) > 0 { // a member of the pack, by name
			opts["ref"] = PackScheme + name + "#"
		}

		if ref, ok := opts["ref"]; ok {
			repl := &jsonschema.Schema{Ref: ref}
			if _, ok := opts["default"]; ok {
				// A key's default layers over the component's own (schema.Defaults
				// merges through a $ref), so only what the author actually set is
				// emitted: zero values would overwrite the component's defaults.
				repl.Default = prune(fv.Interface())
			}
			if mount, ok := opts["mount"]; ok {
				extras(repl)["x-pack-mount"] = mount
			}
			if key := keyAnnotation(opts); key != nil {
				extras(repl)["x-pack"] = key
			}
			s.Properties.Set(name, repl)
			continue
		}
		if key := keyAnnotation(opts); key != nil {
			extras(prop)["x-pack"] = key
		}
		if _, ok := opts["const"]; ok {
			prop.Const = fv.Interface()
		}
		if _, ok := opts["default"]; ok {
			prop.Default = fv.Interface()
		}
		if p, ok := opts["file"]; ok {
			_, per := opts["perInstance"]
			extras(prop)["x-pack-file"] = File{
				Path: p, Format: opts["format"], Template: opts["template"],
				ParseAs: opts["parseAs"], Mode: opts["mode"], PerInstance: per,
			}
		}
		if flag, ok := opts["arg"]; ok {
			_, each := opts["each"]
			_, omit := opts["omitEmpty"]
			extras(prop)["x-pack-arg"] = Arg{Flag: flag, Prefix: opts["prefix"], Each: each, OmitEmpty: omit}
		}
		if env, ok := opts["env"]; ok {
			extras(prop)["x-pack-env"] = env
		}
		if err := annotate(prop, fv, path+"/"+name); err != nil {
			return err
		}
	}
	return nil
}

// prune drops zero values — "", 0, false, nil, empty objects and arrays —
// from a value after marshalling it, leaving what the author set.
func prune(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return v
	}
	out, _ := pruneValue(doc)
	return out
}

func pruneValue(v any) (any, bool) {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			if pe, keep := pruneValue(e); keep {
				out[k] = pe
			}
		}
		return out, len(out) > 0
	case []any:
		return x, len(x) > 0
	case string:
		return x, len(x) > 0
	case float64:
		return x, x != 0
	case bool:
		return x, x
	case nil:
		return nil, false
	}
	return v, true
}

// keyAnnotation builds the x-pack of a composition key from its tag, or nil
// when the tag carries none of its fields.
func keyAnnotation(opts map[string]string) *Key {
	_, spread := opts["spread"]
	peers, hasPeers := opts["peers"]
	after, hasAfter := opts["after"]
	minimum, hasMin := opts["minimum"]
	if !spread && !hasPeers && !hasAfter && !hasMin {
		return nil
	}
	k := &Key{Spread: spread}
	if hasPeers {
		k.Peers = map[string]string{}
		for pair := range strings.SplitSeq(peers, ";") {
			role, key, _ := strings.Cut(pair, ":")
			k.Peers[role] = key
		}
	}
	if hasAfter {
		k.After = strings.Split(after, ";")
	}
	if hasMin {
		k.Minimum = map[string]string{}
		for pair := range strings.SplitSeq(minimum, ";") {
			res, qty, _ := strings.Cut(pair, ":")
			k.Minimum[res] = qty
		}
	}
	return k
}

func lookup(s *jsonschema.Schema, ptr string) (*jsonschema.Schema, error) {
	node := s
	for seg := range strings.SplitSeq(strings.TrimPrefix(ptr, "/"), "/") {
		if node.Properties == nil {
			return nil, fmt.Errorf("%q: not an object", seg)
		}
		next, ok := node.Properties.Get(seg)
		if !ok {
			return nil, fmt.Errorf("%q: no such property", seg)
		}
		node = next
	}
	return node, nil
}

func extras(s *jsonschema.Schema) map[string]any {
	if s.Extras == nil {
		s.Extras = map[string]any{}
	}
	return s.Extras
}

// jsonTagName is the name part of the json tag, "" when absent.
func jsonTagName(f reflect.StructField) string {
	tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return tag
}

func jsonName(f reflect.StructField) string {
	tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if tag == "-" {
		return ""
	}
	if len(tag) == 0 {
		return f.Name
	}
	return tag
}

func parseTag(tag string) map[string]string {
	out := map[string]string{}
	for item := range strings.SplitSeq(tag, ",") {
		if len(item) == 0 {
			continue
		}
		k, v, _ := strings.Cut(item, "=")
		out[k] = v
	}
	return out
}

// JSONSchema makes a Secret leaf reflect as a password string.
func (Secret) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Format: "password"}
}
