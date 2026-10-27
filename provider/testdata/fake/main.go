// fake is a provider for tests: target "fake", which performs steps by
// writing them down. PACK_FAKE_DIR holds its journal (log) and the record
// pack keeps with it (record.json); PACK_FAKE_FAIL names a step to fail, as
// the journal writes it ("apply kafka").
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ks-tool/pack/provider"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

type fake struct{}

func (fake) Name() string           { return "fake" }
func (fake) Capabilities() []string { return []string{target.CapExecute} }

// Configure writes down the set it got (settings.json), for tests to see.
func (fake) Configure(installation string, s target.Settings) (target.Installation, error) {
	dir := s.Get("fake_dir", os.Getenv("PACK_FAKE_DIR"))
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), raw, 0o600); err != nil {
		return nil, err
	}
	return &inst{name: installation, dir: dir}, nil
}

type inst struct{ name, dir string }

func (i *inst) Namespace() string                                { return "" }
func (i *inst) PlatformSchema() []byte                           { return nil }
func (i *inst) ValidatePlatform(string, map[string]any) []string { return nil }

// Resolve: replicas from the values (int64 over the wire), 1 by default.
func (i *inst) Resolve(key, scale string, p map[string]any, _ bool) (*render.Resolved, error) {
	svc := i.name + "-" + key
	n := int64(1)
	if c, ok := p["replicas"].(int64); ok {
		n = c
	}
	res := &render.Resolved{Service: svc, Address: svc, Platform: map[string]any{"replicas": n}}
	if scale == "replicas" {
		for k := range n {
			res.Instances = append(res.Instances, render.Instance{Name: fmt.Sprintf("%s-%d", svc, k), Address: fmt.Sprintf("%s-%d", svc, k)})
		}
	}
	return res, nil
}

func (i *inst) Convert(rel *render.Release, _ []render.Credential) (*target.Output, error) {
	return &target.Output{}, nil
}

func (i *inst) ReadInstalled() ([]byte, bool, error) {
	b, err := os.ReadFile(filepath.Join(i.dir, "record.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func (i *inst) WriteInstalled(r []byte) error {
	return os.WriteFile(filepath.Join(i.dir, "record.json"), r, 0o600)
}

func (i *inst) Execute(_ context.Context, s target.Step, events func(string)) error {
	what := s.Kind + " " + s.Key
	if s.Rollback {
		what = "undo " + what
	}
	if s.Unit != nil && s.Kind == "apply" {
		what += " " + s.Unit.Image
	}
	f, err := os.OpenFile(filepath.Join(i.dir, "log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintln(f, what)
	events("fake: " + what)
	if what == os.Getenv("PACK_FAKE_FAIL") || (s.Kind+" "+s.Key) == os.Getenv("PACK_FAKE_FAIL") && !s.Rollback {
		return errors.New("fake failure")
	}
	return nil
}

func main() {
	if err := provider.Serve(fake{}); err != nil {
		fmt.Fprintln(os.Stderr, "pack-provider-fake:", err)
		os.Exit(1)
	}
}
