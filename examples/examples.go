// Package examples holds the components and the composition the tool is
// developed against. Each is a Go value; `go generate` turns it into
// schema.json beside its source.
//
//go:generate go run ./gen .
package examples

import (
	"encoding/json"

	"github.com/ks-tool/pack/examples/adh"
	"github.com/ks-tool/pack/examples/jdk"
	"github.com/ks-tool/pack/examples/kafka"
	"github.com/ks-tool/pack/examples/postgres"
	"github.com/ks-tool/pack/examples/zookeeper"
	"github.com/ks-tool/pack/sdk/packschema"
)

// All returns every example by name — the directory its schema.json lives in.
func All() map[string]any {
	return map[string]any{
		"jdk":       jdk.Defaults(),
		"zookeeper": zookeeper.Defaults(),
		"kafka":     kafka.Defaults(),
		"postgres":  postgres.Defaults(),
		"adh":       adh.Defaults(),
	}
}

// Schema renders one example's schema.json, stable byte for byte.
func Schema(defaults any) ([]byte, error) {
	s, err := packschema.Reflect(defaults)
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}
