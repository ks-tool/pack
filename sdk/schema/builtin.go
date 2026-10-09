package schema

import (
	"fmt"

	"github.com/ks-tool/pack/sdk/platform"
)

// AttachBuiltins gives a schema the two resources the CLI ships: the platform
// slot (open — a target fills it when a render validates) and the pack
// vocabulary's definitions. Every component needs them.
func (s *Schema) AttachBuiltins() error {
	for _, b := range []struct {
		url string
		raw []byte
	}{{platform.SlotID, platform.Slot}, {platform.PackID, platform.PackV1}} {
		if _, done := s.Foreign[b.url]; done {
			continue
		}
		f, err := Parse(b.raw)
		if err != nil {
			return fmt.Errorf("builtin %s: %w", b.url, err)
		}
		f.Foreign = s.Foreign // share the table, so the platform's own refs resolve
		s.Foreign[b.url] = f
	}
	return nil
}
