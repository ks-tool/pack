package main

import (
	"context"
	"os"
	"strings"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/registry"
)

// openPack loads a composition or a lone component from a directory or,
// when the argument is a registry reference (oci://host/repo:tag, or
// host/repo:tag), straight from the registry — schemas, templates and image
// configs only, never an image layer. Credentials come from
// PACK_REGISTRY_USER / PACK_REGISTRY_PASSWORD.
func openPack(ref string, plainHTTP bool) (*bundle.Bundle, error) {
	if !registry.IsRef(ref) {
		return bundle.Load(ref)
	}
	if _, err := os.Stat(ref); err == nil && !strings.HasPrefix(ref, registry.Scheme) {
		return bundle.Load(ref) // a directory that happens to look like host/repo
	}
	c := &registry.Client{PlainHTTP: plainHTTP, Username: os.Getenv("PACK_REGISTRY_USER"), Password: os.Getenv("PACK_REGISTRY_PASSWORD")}
	return bundle.LoadRef(context.Background(), c, ref)
}
