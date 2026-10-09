package provider

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
	"github.com/ks-tool/pack/target/horchestra"
)

// buildProvider builds cmd/pack-provider-horchestra with the toolchain running the test.
func buildProvider(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the provider binary")
	}
	bin := filepath.Join(t.TempDir(), Prefix+"horchestra")
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", bin, "../cmd/pack-provider-horchestra")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func adh(t *testing.T) (*bundle.Bundle, render.Values) {
	t.Helper()
	root := filepath.Join("..", "examples", "adh")
	b, err := bundle.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "testdata", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var v render.Values
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return b, v
}

// The horchestra provider as a separate process renders adh to exactly the
// golden files — the release, the platform and the conversion all crossing
// the wire — and a provider asked to be a target it is not is refused.
func TestProvider_ADH(t *testing.T) {
	bin := buildProvider(t)

	if _, err := Start("k8s", bin); err == nil || !strings.Contains(err.Error(), `is target "horchestra"`) {
		t.Errorf("a provider of another target must be refused: %v", err)
	}

	p, err := Start("horchestra", bin)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Configure("prod", target.Settings{"namespce": "data"}); err == nil || !strings.Contains(err.Error(), "unknown setting namespce") {
		t.Errorf("the provider's refusal must reach pack: %v", err)
	}
	inst, err := p.Configure("prod", target.Settings{"namespace": "data"})
	if err != nil {
		t.Fatal(err)
	}
	b, v := adh(t)
	rel, err := render.Render(b, v, render.Context{Installation: "prod", Target: inst})
	if err != nil {
		t.Fatal(err)
	}
	if got := rel.Report.Computed["/kafka/jdk/heapMB"]; got != int64(24576) {
		t.Errorf("limits must cross the wire as integers: heapMB = %v (%T)", got, got)
	}
	out, err := inst.Convert(rel, nil)
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join("..", "examples", "adh", "testdata", "expected")
	seen := 0
	for _, f := range out.Files {
		want, err := os.ReadFile(filepath.Join(expected, f.Path))
		if err != nil {
			t.Errorf("%s: %v", f.Path, err)
			continue
		}
		if string(want) != string(f.Data) {
			t.Errorf("%s differs from golden:\n%s", f.Path, f.Data)
		}
		seen++
	}
	if seen < 3 {
		t.Errorf("only %d files came back", seen)
	}
}

// The wire is lossless for what a target reads: a release converted in
// process and the same release sent through the protocol's types convert to
// the same bytes.
func TestReleaseRoundTrip(t *testing.T) {
	b, v := adh(t)
	ctx := horchestra.Context{Installation: "prod", Namespace: "data"}
	rel, err := render.Render(b, v, render.Context{Installation: "prod", Target: ctx.Target()})
	if err != nil {
		t.Fatal(err)
	}
	pb, err := releaseTo(rel)
	if err != nil {
		t.Fatal(err)
	}
	back, err := releaseFrom(pb)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := horchestra.Convert(rel, ctx)
	if err != nil {
		t.Fatal(err)
	}
	wired, err := horchestra.Convert(back, ctx)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := yaml.Marshal(direct)
	c, _ := yaml.Marshal(wired)
	if string(a) != string(c) {
		t.Errorf("a release through the wire converts differently:\n%s\n---\n%s", a, c)
	}
}
