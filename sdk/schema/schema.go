// Package schema is the tool's own view of a JSON Schema: the tree with the
// x-pack-* annotations read off its nodes, defaults gathered from it, and the
// validating view compiled from it. A validator ignores what it does not know,
// so the vocabulary is read here and only here.
package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/ks-tool/pack/sdk/packschema"
)

// Node is one schema object. Only the keywords the tool acts on are lifted out
// of Raw; Raw keeps everything for the validator.
type Node struct {
	Ref                  string
	Types                []string
	Properties           map[string]*Node
	Order                []string // property names in document order
	AllOf                []*Node
	Items                *Node
	Default              any
	HasDefault           bool
	Const                any
	HasConst             bool
	Format               string
	Enum                 []any
	Required             []string
	AdditionalProperties *bool

	Compute string
	File    *packschema.File
	Arg     *packschema.Arg
	Env     string
	Mount   string
	XPack   json.RawMessage // x-pack: Root on a component root, Key on a composition key

	Raw map[string]any
}

type Schema struct {
	Root *Node
	Defs map[string]*Node
	Raw  map[string]any
	// Foreign schemas by resource URL — the part of a $ref before '#':
	// horchestra://platform/v1, horchestra://pack/v1, pack://<name>.
	Foreign map[string]*Schema
}

// SplitRef separates a $ref into its resource URL and fragment.
func SplitRef(ref string) (url, fragment string) {
	if i := strings.IndexByte(ref, '#'); i >= 0 {
		return ref[:i], ref[i:]
	}
	return ref, ""
}

// Parse reads a schema document, lifting the keywords the tool uses and
// decoding every x-pack-* annotation strictly — an unknown key inside one is
// a refusal, since nothing else would ever notice it.
func Parse(raw []byte) (*Schema, error) {
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	s := &Schema{Raw: doc, Defs: map[string]*Node{}, Foreign: map[string]*Schema{}}
	orders, err := propertyOrders(raw)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	root, err := parseNodeOrdered(doc, "", orders)
	if err != nil {
		return nil, err
	}
	s.Root = root
	if defs, ok := doc["$defs"].(map[string]any); ok {
		for name, d := range defs {
			dm, ok := d.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("$defs/%s: not an object", name)
			}
			n, err := parseNodeOrdered(dm, "/$defs/"+name, orders)
			if err != nil {
				return nil, err
			}
			s.Defs[name] = n
		}
	}
	return s, nil
}

func parseNodeOrdered(m map[string]any, ptr string, orders map[string][]string) (*Node, error) {
	n := &Node{Raw: m}
	if r, ok := m["$ref"].(string); ok {
		n.Ref = r
	}
	switch t := m["type"].(type) {
	case string:
		n.Types = []string{t}
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok {
				n.Types = append(n.Types, s)
			}
		}
	}
	if d, ok := m["default"]; ok {
		n.Default, n.HasDefault = d, true
	}
	if c, ok := m["const"]; ok {
		n.Const, n.HasConst = c, true
	}
	if f, ok := m["format"].(string); ok {
		n.Format = f
	}
	if e, ok := m["enum"].([]any); ok {
		n.Enum = e
	}
	if r, ok := m["required"].([]any); ok {
		for _, x := range r {
			if s, ok := x.(string); ok {
				n.Required = append(n.Required, s)
			}
		}
	}
	if ap, ok := m["additionalProperties"].(bool); ok {
		n.AdditionalProperties = &ap
	}
	if props, ok := m["properties"].(map[string]any); ok {
		n.Properties = map[string]*Node{}
		for name, p := range props {
			pm, ok := p.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s/properties/%s: not an object", ptr, name)
			}
			child, err := parseNodeOrdered(pm, ptr+"/properties/"+escape(name), orders)
			if err != nil {
				return nil, err
			}
			n.Properties[name] = child
		}
		if o, ok := orders[ptr+"/properties"]; ok {
			n.Order = o
		} else {
			n.Order = sortedKeys(props)
		}
	}
	if all, ok := m["allOf"].([]any); ok {
		for i, a := range all {
			am, ok := a.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s/allOf/%d: not an object", ptr, i)
			}
			child, err := parseNodeOrdered(am, fmt.Sprintf("%s/allOf/%d", ptr, i), orders)
			if err != nil {
				return nil, err
			}
			n.AllOf = append(n.AllOf, child)
		}
	}
	if items, ok := m["items"].(map[string]any); ok {
		child, err := parseNodeOrdered(items, ptr+"/items", orders)
		if err != nil {
			return nil, err
		}
		n.Items = child
	}
	// annotations — strict
	if c, ok := m["x-pack-compute"]; ok {
		s, ok := c.(string)
		if !ok || len(s) == 0 {
			return nil, fmt.Errorf("%s: x-pack-compute must be a non-empty string", ptr)
		}
		n.Compute = s
	}
	if f, ok := m["x-pack-file"]; ok {
		n.File = &packschema.File{}
		if err := strict(f, n.File); err != nil {
			return nil, fmt.Errorf("%s: x-pack-file: %w", ptr, err)
		}
	}
	if a, ok := m["x-pack-arg"]; ok {
		n.Arg = &packschema.Arg{}
		if err := strict(a, n.Arg); err != nil {
			return nil, fmt.Errorf("%s: x-pack-arg: %w", ptr, err)
		}
	}
	if e, ok := m["x-pack-env"]; ok {
		s, ok := e.(string)
		if !ok || len(s) == 0 {
			return nil, fmt.Errorf("%s: x-pack-env must be a non-empty string", ptr)
		}
		n.Env = s
	}
	if mnt, ok := m["x-pack-mount"]; ok {
		s, ok := mnt.(string)
		if !ok || !strings.HasPrefix(s, "/") {
			return nil, fmt.Errorf("%s: x-pack-mount must be an absolute path", ptr)
		}
		n.Mount = s
	}
	if x, ok := m["x-pack"]; ok {
		b, err := json.Marshal(x)
		if err != nil {
			return nil, err
		}
		n.XPack = b
	}
	return n, nil
}

// strict decodes an annotation into its type, refusing unknown keys.
func strict(v any, into any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// RootAnnotation decodes x-pack of a component root.
func (n *Node) RootAnnotation() (*packschema.Root, error) {
	if n.XPack == nil {
		return &packschema.Root{}, nil
	}
	r := &packschema.Root{}
	dec := json.NewDecoder(bytes.NewReader(n.XPack))
	dec.DisallowUnknownFields()
	if err := dec.Decode(r); err != nil {
		return nil, fmt.Errorf("x-pack: %w", err)
	}
	return r, nil
}

// KeyAnnotation decodes x-pack of a composition key.
func (n *Node) KeyAnnotation() (*packschema.Key, error) {
	if n.XPack == nil {
		return &packschema.Key{}, nil
	}
	k := &packschema.Key{}
	dec := json.NewDecoder(bytes.NewReader(n.XPack))
	dec.DisallowUnknownFields()
	if err := dec.Decode(k); err != nil {
		return nil, fmt.Errorf("x-pack: %w", err)
	}
	return k, nil
}

func (n *Node) Is(t string) bool {
	return slices.Contains(n.Types, t)
}

func (n *Node) IsPassword() bool { return n.Format == "password" }

// IsForeign reports whether the $ref points outside this document.
func (n *Node) IsForeign() bool {
	return len(n.Ref) > 0 && !strings.HasPrefix(n.Ref, "#")
}

// Deref follows a $ref: an internal one within s, a foreign one into the
// schema attached under its resource URL. It returns the schema the target
// lives in. Fragments are "", "#" (the root) or "#/$defs/<name>".
func (s *Schema) Deref(n *Node) (*Schema, *Node, error) {
	if len(n.Ref) == 0 {
		return s, n, nil
	}
	url, fragment := SplitRef(n.Ref)
	target := s
	if len(url) > 0 {
		f, ok := s.Foreign[url]
		if !ok {
			return nil, nil, fmt.Errorf("$ref %q: resource %q is not attached", n.Ref, url)
		}
		target = f
	}
	switch {
	case len(fragment) == 0 || fragment == "#":
		return target, target.Root, nil
	case strings.HasPrefix(fragment, "#/$defs/"):
		name := strings.TrimPrefix(fragment, "#/$defs/")
		d, ok := target.Defs[name]
		if !ok {
			return nil, nil, fmt.Errorf("$ref %q: no definition %q", n.Ref, name)
		}
		return target, d, nil
	}
	return nil, nil, fmt.Errorf("$ref %q: only '#' and '#/$defs/<name>' fragments are supported", n.Ref)
}

// Attach registers a foreign schema under its resource URL, for this schema
// and, transitively, for the builtins it needs.
func (s *Schema) Attach(url string, f *Schema) { s.Foreign[url] = f }

// Visitor visit walks every node reachable from the root along properties, allOf and
// items, following $refs (a foreign one continues in the foreign schema), and
// reports each with the JSON Pointer of the INSTANCE it describes. A node is
// visited once per instance path, so a $ref used twice is visited twice.
type Visitor func(ptr string, s *Schema, n *Node) error

func (s *Schema) Visit(fn Visitor) error { return s.visit(s.Root, "", fn, 0) }

func (s *Schema) visit(n *Node, ptr string, fn Visitor, depth int) error {
	if depth > 64 {
		return fmt.Errorf("%s: $ref chain too deep", ptr)
	}
	if err := fn(ptr, s, n); err != nil {
		return err
	}
	if len(n.Ref) > 0 {
		ts, tn, err := s.Deref(n)
		if err != nil {
			return fmt.Errorf("%s: %w", ptr, err)
		}
		return ts.visit(tn, ptr, fn, depth+1)
	}
	for _, name := range n.Order {
		if err := s.visit(n.Properties[name], ptr+"/"+escape(name), fn, depth+1); err != nil {
			return err
		}
	}
	for _, a := range n.AllOf {
		if err := s.visit(a, ptr, fn, depth+1); err != nil {
			return err
		}
	}
	if n.Items != nil {
		if err := s.visit(n.Items, ptr+"/-", fn, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// Lookup finds the node describing an instance pointer, following $refs.
func (s *Schema) Lookup(ptr string) (*Schema, *Node, error) {
	cur, curS := s.Root, s
	for _, seg := range split(ptr) {
		var err error
		if curS, cur, err = curS.Deref(cur); err != nil {
			return nil, nil, err
		}
		next, ok := cur.Properties[seg]
		if !ok {
			return nil, nil, fmt.Errorf("%s: no property %q", ptr, seg)
		}
		cur = next
	}
	return curS.Deref(cur)
}

// Defaults builds the defaults document (spec §4.2): a node's default or const
// is taken whole; objects gather their properties' defaults; allOf branches are
// merged, a conflict on one path being an error; arrays contribute only an
// explicit default; anyOf/oneOf/if contribute nothing.
func (s *Schema) Defaults() (any, bool, error) { return s.defaults(s.Root, "", 0) }

func (s *Schema) defaults(n *Node, ptr string, depth int) (any, bool, error) {
	if depth > 64 {
		return nil, false, fmt.Errorf("%s: $ref chain too deep", ptr)
	}
	var base any
	var ok bool
	if len(n.Ref) > 0 {
		ts, tn, err := s.Deref(n)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", ptr, err)
		}
		if base, ok, err = ts.defaults(tn, ptr, depth+1); err != nil {
			return nil, false, err
		}
	}
	if n.HasDefault {
		// A default on a $ref node layers over what the target defaults to —
		// a composition key that sets replicas must not erase the component's
		// own defaults. Elsewhere a default is the node, whole.
		if bm, isMap := base.(map[string]any); ok && isMap {
			if dm, isMap := n.Default.(map[string]any); isMap {
				out := deepCopy(bm).(map[string]any)
				overlay(out, deepCopy(dm).(map[string]any))
				return out, true, nil
			}
		}
		return deepCopy(n.Default), true, nil
	}
	if n.HasConst {
		return deepCopy(n.Const), true, nil
	}
	if n.Is("object") || len(n.Properties) > 0 || len(n.AllOf) > 0 {
		out := map[string]any{}
		if bm, isMap := base.(map[string]any); isMap && ok {
			out = bm
		}
		for _, a := range n.AllOf {
			v, ok, err := s.defaults(a, ptr, depth+1)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				continue
			}
			vm, isMap := v.(map[string]any)
			if !isMap {
				return nil, false, fmt.Errorf("%s: allOf branch defaults to a non-object", ptr)
			}
			if err := mergeDefaults(out, vm, ptr); err != nil {
				return nil, false, err
			}
		}
		names := make([]string, 0, len(n.Properties))
		for name := range n.Properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			v, ok, err := s.defaults(n.Properties[name], ptr+"/"+escape(name), depth+1)
			if err != nil {
				return nil, false, err
			}
			if ok {
				out[name] = v
			}
		}
		return out, len(out) > 0, nil
	}
	return base, ok, nil
}

func mergeDefaults(dst, src map[string]any, ptr string) error {
	for k, v := range src {
		if cur, exists := dst[k]; exists {
			cm, cok := cur.(map[string]any)
			vm, vok := v.(map[string]any)
			if cok && vok {
				if err := mergeDefaults(cm, vm, ptr+"/"+escape(k)); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("%s/%s: two allOf branches default it differently", ptr, escape(k))
		}
		dst[k] = v
	}
	return nil
}

// overlay writes src over dst, descending into objects; arrays and scalars
// are replaced whole.
func overlay(dst, src map[string]any) {
	for k, v := range src {
		if dm, ok := dst[k].(map[string]any); ok {
			if sm, ok := v.(map[string]any); ok {
				overlay(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

// Overlay is overlay for other packages.
func Overlay(dst, src map[string]any) { overlay(dst, src) }

// DeepCopy is deepCopy for other packages.
func DeepCopy(v any) any { return deepCopy(v) }

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopy(e)
		}
		return out
	}
	return v
}

func sortedKeys(m map[string]any) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// propertyOrders scans the document's tokens once and records, for every
// object under a "properties" key, the order its member names were written in
// — keyed by that object's JSON Pointer within the SCHEMA document. Argv is
// assembled in author order, and the author's order is what error messages
// should follow; encoding/json's map forgets it.
func propertyOrders(raw []byte) (map[string][]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	orders := map[string][]string{}
	var walk func(ptr string) error
	walk = func(ptr string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch d := tok.(type) {
		case json.Delim:
			switch d {
			case '{':
				isProps := strings.HasSuffix(ptr, "/properties")
				for dec.More() {
					kt, err := dec.Token()
					if err != nil {
						return err
					}
					key := kt.(string)
					if isProps {
						orders[ptr] = append(orders[ptr], key)
					}
					if err := walk(ptr + "/" + escape(key)); err != nil {
						return err
					}
				}
				_, err = dec.Token() // '}'
				return err
			case '[':
				for i := 0; dec.More(); i++ {
					if err := walk(fmt.Sprintf("%s/%d", ptr, i)); err != nil {
						return err
					}
				}
				_, err = dec.Token() // ']'
				return err
			}
		}
		return nil
	}
	return orders, walk("")
}

func escape(seg string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(seg)
}

func unescape(seg string) string {
	return strings.NewReplacer("~1", "/", "~0", "~").Replace(seg)
}

func split(ptr string) []string {
	if len(ptr) == 0 || ptr == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(ptr, "/"), "/")
	for i := range parts {
		parts[i] = unescape(parts[i])
	}
	return parts
}

// Pointer joins segments into a JSON Pointer.
func Pointer(segs ...string) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString("/")
		b.WriteString(escape(s))
	}
	return b.String()
}

// Split exposes pointer splitting to other packages.
func Split(ptr string) []string { return split(ptr) }
