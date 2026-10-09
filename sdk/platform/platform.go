// Package platform carries what the CLI ships as blobs of its own release:
// the pack vocabulary (horchestra://pack/v1), generated from the Go types in
// packschema, and the platform SLOT — the reference a component's `platform`
// key makes to whatever schema the target gives it.
//
//go:generate go run ./gen
package platform

import _ "embed"

//go:embed pack_v1.json
var PackV1 []byte

const (
	PackID    = "horchestra://pack/v1"
	SecretRef = "horchestra://pack/v1#/$defs/secretRef"
	// SlotID is what a component's `platform` key refers to. No schema of
	// the pack fills it: a target does (its words — replicas, resources,
	// placement …), and without one the slot takes any object.
	SlotID = "urn:pack:platform"
)

// Slot is the slot's schema when no target fills it.
var Slot = []byte(`{"$schema": "https://json-schema.org/draft/2020-12/schema", "$id": "urn:pack:platform", "type": "object"}`)
