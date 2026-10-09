// Package platform carries what the CLI ships as blobs of its own release:
// the pack vocabulary (horchestra://pack/v1), generated from the Go types in
// packschema, and the pack's half of the platform SLOT — the schema a
// component's `platform` key refers to.
//
// The slot is two schemas made one. The pack's half (PlatformV1) says what is
// the same on every target: resources — cgroups, whether Kubernetes,
// horchestra or a VM sets them — env and metadata. The target's half comes
// from its provider: replicas, placement, whatever its scheduler has words
// for; pack schedules nothing. Merge joins them.
//
//go:generate go run ./gen
package platform

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

//go:embed pack_v1.json
var PackV1 []byte

// PlatformV1 is the pack's half of the platform slot.
//
//go:embed platform_v1.json
var PlatformV1 []byte

const (
	PackID    = "horchestra://pack/v1"
	SecretRef = "horchestra://pack/v1#/$defs/secretRef"
	// SlotID is what a component's `platform` key refers to.
	SlotID = "urn:pack:platform"
)

// Slot is the slot without a target: the pack's half, and any property a
// target might add.
var Slot = mustMerge(nil)

// Merge makes the slot of the pack's half and a target's: their properties,
// side by side — a target redefining one of the pack's is refused — and
// nothing else. A nil target schema leaves the slot open to properties the
// pack does not know.
func Merge(target []byte) ([]byte, error) {
	var pack map[string]any
	if err := json.Unmarshal(PlatformV1, &pack); err != nil {
		return nil, fmt.Errorf("the pack's platform schema: %w", err)
	}
	props, _ := pack["properties"].(map[string]any)
	out := map[string]any{
		"$schema":    pack["$schema"],
		"$id":        SlotID,
		"type":       "object",
		"properties": props,
	}
	if target == nil {
		out["additionalProperties"] = true
		return json.Marshal(out)
	}
	var t map[string]any
	if err := json.Unmarshal(target, &t); err != nil {
		return nil, fmt.Errorf("the target's platform schema: %w", err)
	}
	tprops, _ := t["properties"].(map[string]any)
	var clash []string
	for name, p := range tprops {
		if _, ok := props[name]; ok {
			clash = append(clash, name)
			continue
		}
		props[name] = p
	}
	if len(clash) > 0 {
		sort.Strings(clash)
		return nil, fmt.Errorf("the target's platform schema redefines the pack's %v", clash)
	}
	if defs, ok := t["$defs"]; ok {
		out["$defs"] = defs
	}
	if req, ok := t["required"]; ok {
		out["required"] = req
	}
	out["additionalProperties"] = false
	return json.Marshal(out)
}

func mustMerge(target []byte) []byte {
	b, err := Merge(target)
	if err != nil {
		panic(err)
	}
	return b
}
