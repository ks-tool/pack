package render

import (
	"fmt"
	"strings"

	"github.com/ks-tool/pack/sdk/schema"
)

// get reads a JSON Pointer from a document.
func get(doc any, ptr string) (any, bool) {
	cur := doc
	for _, seg := range schema.Split(ptr) {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// set writes a JSON Pointer into a document, creating objects on the way.
func set(doc map[string]any, ptr string, v any) error {
	segs := schema.Split(ptr)
	if len(segs) == 0 {
		return fmt.Errorf("cannot set the root")
	}
	cur := doc
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg]
		if !ok {
			m := map[string]any{}
			cur[seg] = m
			cur = m
			continue
		}
		m, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: %q is not an object", ptr, seg)
		}
		cur = m
	}
	cur[segs[len(segs)-1]] = v
	return nil
}

// pointers lists every leaf pointer present in a document — what the user
// wrote, so that a formula never overrides it. An empty object writes
// nothing: values overlay the defaults deep, {} clears nothing, and a key
// named as {} only to select it keeps its formulas.
func pointers(v any, prefix string, out map[string]bool) {
	m, ok := v.(map[string]any)
	if !ok {
		out[prefix] = true
		return
	}
	for k, e := range m {
		pointers(e, prefix+schema.Pointer(k), out)
	}
}

func dotted(ptr string) string { return strings.ReplaceAll(strings.TrimPrefix(ptr, "/"), "/", ".") }
