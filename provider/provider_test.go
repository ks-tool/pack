package provider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/environment"
	"github.com/ks-tool/pack/provider/providerpb"
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
	env, err := environment.Load(context.Background(), filepath.Join("..", "examples", "adh", "envs", "horchestra.yaml"), map[string]string{"registry_user": "ci"})
	if err != nil {
		t.Fatal(err)
	}
	// a setting the target has not is refused across the wire
	if _, err := p.Configure("prod", target.Settings(env.Settings)); err == nil || !strings.Contains(err.Error(), "no setting registry_user") {
		t.Errorf("registry_user: %v", err)
	}
	delete(env.Settings, "registry_user")
	inst, err := p.Configure("prod", target.Settings(env.Settings))
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
	expected := filepath.Join("..", "target", "horchestra", "testdata", "adh", "expected")
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

// A provider on TCP — as in a container — answers pack, which carries the
// token, and nobody else.
func TestProvider_TCPAndToken(t *testing.T) {
	bin := buildProvider(t)
	p, err := startTCP("horchestra", bin)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Configure("prod", target.Settings{"namespace": "data"}); err != nil {
		t.Fatalf("pack, with the token: %v", err)
	}
	conn, err := grpc.NewClient(p.conn.Target(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = providerpb.NewProviderClient(conn).Handshake(context.Background(), &providerpb.HandshakeRequest{Protocol: Protocol})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("a call without the token must be refused: %v", err)
	}
}

// The test provider as a container image, run by the machine's runtime:
// pack starts it, configures it, has it perform a step and keep the record.
func TestProvider_Image(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a container image")
	}
	rt, err := containerRuntime()
	if err != nil {
		t.Skip(err)
	}
	if out, err := exec.Command(rt, "info").CombinedOutput(); err != nil {
		t.Skipf("%s is not running: %s", rt, out)
	}
	ctx := t.TempDir()
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", filepath.Join(ctx, "provider"), "./testdata/fake")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	dockerfile := "FROM scratch\nCOPY provider /provider\nCOPY tmp /tmp\nENV PACK_FAKE_DIR=/tmp\nENTRYPOINT [\"/provider\"]\n"
	if err := os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ctx, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	image := "localhost/pack-provider-fake:test"
	if out, err := exec.Command(rt, "build", "-q", "-t", image, ctx).CombinedOutput(); err != nil {
		t.Skipf("cannot build a test image: %v\n%s", err, out)
	}
	defer exec.Command(rt, "rmi", "-f", image).Run()

	p, err := Open("fake", "oci://"+image)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	inst, err := p.Configure("prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	x, ok := inst.(target.Executor)
	if !ok {
		t.Fatal("the fake provider declares execute")
	}
	var events []string
	if err := x.Execute(context.Background(), target.Step{N: 1, Key: "kafka", Kind: "apply"}, func(s string) { events = append(events, s) }); err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || !strings.Contains(events[0], "apply kafka") {
		t.Errorf("events: %v", events)
	}
	if err := x.WriteInstalled([]byte(`{"state":"installed"}`)); err != nil {
		t.Fatal(err)
	}
	rec, exists, err := x.ReadInstalled()
	if err != nil || !exists || string(rec) != `{"state":"installed"}` {
		t.Errorf("record: %q %v %v", rec, exists, err)
	}
}
