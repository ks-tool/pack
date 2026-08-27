// Package platform carries the two schemas the CLI ships as blobs of its own
// release: the platform half of a component (horchestra://platform/v1) and the
// pack vocabulary (horchestra://pack/v1), the latter generated from the Go
// types in packschema.
//
//go:generate go run ./gen
package platform

import _ "embed"

//go:embed platform_v1.json
var PlatformV1 []byte

//go:embed pack_v1.json
var PackV1 []byte

const (
	PlatformID = "horchestra://platform/v1"
	PackID     = "horchestra://pack/v1"
	SecretRef  = "horchestra://pack/v1#/$defs/secretRef"
)

// RefURL is the $ref a component uses for its platform half.
func RefURL() string { return PlatformID + "#/$defs/component" }
