package bundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/ks-tool/pack/sdk/packschema"
	"github.com/ks-tool/pack/sdk/schema"
)

// A composition may name another composition under a key: a pack made of
// packs (ADH of Ozone, Impala …, the stand of ADH and ADPS). The system has one
// namespace of keys, so the nested composition is not a key but its keys are:
// they take its place, in its order, and the values stay flat
// (kafka:, ozone-scm:, never adh: {ozone-scm: …}).
//
// A key two compositions both hold — zookeeper under ADH and under ADPS — is
// one key when both name the same component: their annotations are joined
// (after: together, spread: either, peers: no role bound two ways). Two
// components under one key is a contradiction and is refused.
//
// What the nested composition says about the system comes along: its common
// (properties side by side), its savepoints, its hooks, its rollout where the
// outer has none. What the outer says of the key that named it — `after` —
// goes to each of its keys; nothing else of an annotation has a meaning there.

// inline returns raw with every key that names a composition replaced by that
// composition's keys; raw itself when it names none.
func inline(src Source, raw []byte, trail []string) ([]byte, bool, error) {
	d, err := readDoc(raw)
	if err != nil {
		return nil, false, err
	}
	var (
		out     []prop
		at      = map[string]int{}
		changed bool
		x       = d.xpack()
	)
	put := func(p prop) error {
		i, dup := at[p.name]
		switch {
		case !dup:
			at[p.name] = len(out)
			out = append(out, p)
		case p.name == "common":
			m, err := mergeCommon(out[i].raw, p.raw)
			if err != nil {
				return err
			}
			out[i].raw = m
		default:
			m, err := mergeKey(p.name, out[i].raw, p.raw)
			if err != nil {
				return err
			}
			out[i].raw = m
		}
		return nil
	}

	for _, p := range d.props {
		name, ok := refName(p.raw)
		if p.name == "common" || !ok {
			if err := put(p); err != nil {
				return nil, false, err
			}
			continue
		}
		f, err := src.Member(name)
		if err != nil {
			return nil, false, fmt.Errorf("component %q: %w", name, err)
		}
		s, err := schema.Parse(f.Schema)
		if err != nil {
			return nil, false, fmt.Errorf("component %q: %w", name, err)
		}
		if !isComposition(s) {
			if err := put(p); err != nil {
				return nil, false, err
			}
			continue
		}
		if slices.Contains(trail, name) {
			return nil, false, fmt.Errorf("composition %q contains itself: %s → %s", name, strings.Join(trail, " → "), name)
		}
		nested, _, err := inline(src, f.Schema, append(slices.Clone(trail), name))
		if err != nil {
			return nil, false, fmt.Errorf("composition %q: %w", name, err)
		}
		nd, err := readDoc(nested)
		if err != nil {
			return nil, false, err
		}
		after, err := namedByComposition(p.name, p.raw)
		if err != nil {
			return nil, false, err
		}
		for _, np := range nd.props {
			if np.name != "common" && len(after) > 0 {
				if np.raw, err = addAfter(np.name, np.raw, after); err != nil {
					return nil, false, err
				}
			}
			if err := put(np); err != nil {
				return nil, false, fmt.Errorf("composition %q: %w", name, err)
			}
		}
		x.absorb(nd.xpack())
		changed = true
	}
	if !changed {
		return raw, false, nil
	}
	d.top["properties"] = object(out)
	d.top["x-pack"] = x.raw()
	b, err := json.Marshal(d.top)
	return b, true, err
}

// isComposition: nothing in the schema but pack:// references (and common),
// and nothing that makes it a component.
func isComposition(s *schema.Schema) bool {
	keys := 0
	for name, n := range s.Root.Properties {
		if name == "common" {
			continue
		}
		if !n.IsForeign() || !strings.HasPrefix(n.Ref, packschema.PackScheme) {
			return false
		}
		keys++
	}
	return keys > 0 && !isComponent(s)
}

// ---- the document, with its properties in the order they were written

type prop struct {
	name string
	raw  json.RawMessage
}

type doc struct {
	top   map[string]json.RawMessage
	props []prop
}

func readDoc(raw []byte) (*doc, error) {
	d := &doc{}
	if err := json.Unmarshal(raw, &d.top); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	if p, ok := d.top["properties"]; ok {
		var err error
		if d.props, err = ordered(p); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func ordered(raw json.RawMessage) ([]prop, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, fmt.Errorf("properties: not an object")
	}
	var out []prop
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, prop{k.(string), v})
	}
	return out, nil
}

func object(props []prop) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range props {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(p.name)
		b.Write(k)
		b.WriteByte(':')
		b.Write(p.raw)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// refName is the name in a property's `$ref: pack://<name>#`.
func refName(raw json.RawMessage) (string, bool) {
	var n struct {
		Ref string `json:"$ref"`
	}
	if json.Unmarshal(raw, &n) != nil {
		return "", false
	}
	url, _ := schema.SplitRef(n.Ref)
	name, ok := strings.CutPrefix(url, packschema.PackScheme)
	return name, ok && len(name) > 0
}

// ---- what a composition says about the system

type system struct {
	m map[string]any
}

func (d *doc) xpack() *system {
	s := &system{m: map[string]any{}}
	if raw, ok := d.top["x-pack"]; ok {
		_ = json.Unmarshal(raw, &s.m)
	}
	return s
}

func (s *system) raw() json.RawMessage {
	if len(s.m) == 0 {
		return json.RawMessage(`{}`)
	}
	b, _ := json.Marshal(s.m)
	return b
}

// absorb takes from a nested composition what is the system's: savepoints
// and hooks add up, a rollout is the outer's if it has one.
func (s *system) absorb(n *system) {
	if sp, _ := n.m["savepoints"].([]any); len(sp) > 0 {
		have, _ := s.m["savepoints"].([]any)
		for _, k := range sp {
			if !slices.Contains(have, k) {
				have = append(have, k)
			}
		}
		s.m["savepoints"] = have
	}
	if h, _ := n.m["hooks"].([]any); len(h) > 0 {
		have, _ := s.m["hooks"].([]any)
		s.m["hooks"] = append(have, h...)
	}
	if _, has := s.m["rollout"]; !has {
		if r, ok := n.m["rollout"]; ok {
			s.m["rollout"] = r
		}
	}
}

// ---- keys

// namedByComposition is what the outer composition says of the key that names
// a composition: only `after` has a meaning for several keys at once.
func namedByComposition(key string, raw json.RawMessage) ([]string, error) {
	k, err := keyOf(raw)
	if err != nil {
		return nil, fmt.Errorf("key %q: %w", key, err)
	}
	if k.Spread || len(k.Peers) > 0 || len(k.Minimum) > 0 {
		return nil, fmt.Errorf("key %q names a composition: only `after` can be said of a composition's keys at once, not spread, peers or minimum", key)
	}
	return k.After, nil
}

func keyOf(raw json.RawMessage) (packschema.Key, error) {
	var n struct {
		XPack packschema.Key `json:"x-pack"`
	}
	return n.XPack, json.Unmarshal(raw, &n)
}

func addAfter(name string, raw json.RawMessage, after []string) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	k, err := keyOf(raw)
	if err != nil {
		return nil, err
	}
	for _, a := range after {
		if a != name && !slices.Contains(k.After, a) {
			k.After = append(k.After, a)
		}
	}
	if m["x-pack"], err = json.Marshal(k); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// mergeKey joins two holders of one key.
func mergeKey(name string, a, b json.RawMessage) (json.RawMessage, error) {
	var am, bm map[string]json.RawMessage
	if err := json.Unmarshal(a, &am); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &bm); err != nil {
		return nil, err
	}
	ra, _ := refName(a)
	rb, _ := refName(b)
	if ra != rb {
		return nil, fmt.Errorf("key %q is %s in one composition and %s in another", name, ra, rb)
	}
	for f, v := range bm {
		if f == "x-pack" {
			continue
		}
		if w, ok := am[f]; ok && !sameJSON(v, w) {
			return nil, fmt.Errorf("key %q: %s differs between the compositions that hold it", name, f)
		}
		am[f] = v
	}
	ka, err := keyOf(a)
	if err != nil {
		return nil, fmt.Errorf("key %q: %w", name, err)
	}
	kb, err := keyOf(b)
	if err != nil {
		return nil, fmt.Errorf("key %q: %w", name, err)
	}
	for _, x := range kb.After {
		if !slices.Contains(ka.After, x) {
			ka.After = append(ka.After, x)
		}
	}
	ka.Spread = ka.Spread || kb.Spread
	for role, key := range kb.Peers {
		if have, ok := ka.Peers[role]; ok && have != key {
			return nil, fmt.Errorf("key %q: role %q is bound to %q in one composition and %q in another", name, role, have, key)
		}
		if ka.Peers == nil {
			ka.Peers = map[string]string{}
		}
		ka.Peers[role] = key
	}
	for res, min := range kb.Minimum {
		if have, ok := ka.Minimum[res]; ok && have != min {
			return nil, fmt.Errorf("key %q: minimum %s is %s in one composition and %s in another", name, res, have, min)
		}
		if ka.Minimum == nil {
			ka.Minimum = map[string]string{}
		}
		ka.Minimum[res] = min
	}
	if len(ka.After)+len(ka.Peers)+len(ka.Minimum) > 0 || ka.Spread {
		x, err := json.Marshal(ka)
		if err != nil {
			return nil, err
		}
		am["x-pack"] = x
	}
	return json.Marshal(am)
}

// mergeCommon joins two `common` objects: their properties side by side.
func mergeCommon(a, b json.RawMessage) (json.RawMessage, error) {
	var am, bm map[string]json.RawMessage
	if err := json.Unmarshal(a, &am); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &bm); err != nil {
		return nil, err
	}
	ap, err := ordered(am["properties"])
	if err != nil && am["properties"] != nil {
		return nil, err
	}
	bp, err := ordered(bm["properties"])
	if err != nil && bm["properties"] != nil {
		return nil, err
	}
	for _, p := range bp {
		i := slices.IndexFunc(ap, func(x prop) bool { return x.name == p.name })
		switch {
		case i < 0:
			ap = append(ap, p)
		case !sameJSON(ap[i].raw, p.raw):
			return nil, fmt.Errorf("common.%s is defined two ways", p.name)
		}
	}
	if len(ap) > 0 {
		am["properties"] = object(ap)
	}
	return json.Marshal(am)
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}
