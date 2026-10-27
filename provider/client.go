package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/ks-tool/pack/provider/providerpb"
	"github.com/ks-tool/pack/registry"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

const (
	// EnvPath lists directories searched for provider binaries before the
	// directory of pack itself and PATH.
	EnvPath = "PACK_PROVIDER_PATH"
	// Prefix names a provider binary: pack-provider-<target>.
	Prefix = "pack-provider-"

	// EnvRuntime names the container runtime that runs a provider image;
	// unset — podman, else docker, whichever is on PATH.
	EnvRuntime = "PACK_CONTAINER_RUNTIME"

	binaryStart = 10 * time.Second
	imageStart  = 3 * time.Minute // the runtime may pull the image first
	imagePort   = "7353"          // where a provider listens inside its container
)

// Find locates pack-provider-<name>: in PACK_PROVIDER_PATH, beside the
// running pack, then on PATH.
func Find(name string) (string, error) {
	bin := Prefix + name
	var dirs []string
	if p := os.Getenv(EnvPath); len(p) > 0 {
		dirs = append(dirs, filepath.SplitList(p)...)
	}
	if self, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(self))
	}
	for _, d := range dirs {
		p := filepath.Join(d, bin)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	if p, err := exec.LookPath(bin); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("target %q: no %s in %s, beside pack, or on PATH", name, bin, EnvPath)
}

// Plugin is a provider running in its own process, as a target.Target.
// Close it when done: it stops the process.
type Plugin struct {
	name   string
	what   string // the binary or the image, for messages
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	exited chan error // the process's one Wait
	dir    string
	kill   func() // stops what Close could not
	conn   *grpc.ClientConn
	client providerpb.ProviderClient
	caps   []string
}

// Launch finds and starts the provider of a target and shakes hands with it.
func Launch(name string) (*Plugin, error) { return Open(name, "") }

// Open starts the provider of a target from spec: a container image as
// oci://<host>/<repo>:<tag>, a binary by its path, or — empty — the binary
// pack-provider-<name> wherever Find finds it.
func Open(name, spec string) (*Plugin, error) {
	switch {
	case strings.HasPrefix(spec, "oci://"):
		if registry.IsLayout(spec) {
			return nil, fmt.Errorf("target %s: %s is a local OCI layout — a container runtime runs an image from a registry", name, spec)
		}
		return StartImage(name, strings.TrimPrefix(spec, "oci://"))
	case len(spec) > 0:
		return Start(name, spec)
	}
	bin, err := Find(name)
	if err != nil {
		return nil, err
	}
	return Start(name, bin)
}

// Start runs the provider binary at bin for the target name; it listens on
// a unix socket in a private directory.
func Start(name, bin string) (*Plugin, error) {
	dir, err := os.MkdirTemp("", "pack-provider-")
	if err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "provider.sock")
	token, err := newToken()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), EnvAddress+"=unix://"+sock, EnvToken+"="+token)
	p := &Plugin{name: name, what: bin, cmd: cmd, dir: dir}
	return p.start("unix://"+sock, token, binaryStart)
}

// startTCP runs a provider binary listening on TCP, as an image's does — for tests.
func startTCP(name, bin string) (*Plugin, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), EnvAddress+"=tcp://127.0.0.1:"+port, EnvToken+"="+token)
	p := &Plugin{name: name, what: bin, cmd: cmd}
	return p.start("127.0.0.1:"+port, token, binaryStart)
}

// StartImage runs the provider image for the target name in a container:
// podman or docker (PACK_CONTAINER_RUNTIME), the provider listening on TCP
// inside, published on a free port of 127.0.0.1 only. The runtime pulls the
// image when it does not have it.
func StartImage(name, image string) (*Plugin, error) {
	runtime, err := containerRuntime()
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	cname := "pack-provider-" + name + "-" + token[:12]
	cmd := exec.Command(runtime, "run", "-i", "--rm", "--name", cname,
		"-p", "127.0.0.1:"+port+":"+imagePort,
		"-e", EnvAddress+"=tcp://0.0.0.0:"+imagePort, "-e", EnvToken+"="+token,
		image)
	p := &Plugin{name: name, what: image, cmd: cmd,
		kill: func() { _ = exec.Command(runtime, "kill", cname).Run() }}
	return p.start("127.0.0.1:"+port, token, imageStart)
}

// start runs the process and shakes hands with it at addr, retrying until it
// answers, it exits, or the time is up.
func (p *Plugin) start(addr, token string, within time.Duration) (*Plugin, error) {
	p.cmd.Stdout = os.Stderr // a provider has nothing to say on pack's stdout
	p.cmd.Stderr = os.Stderr
	var err error
	if p.stdin, err = p.cmd.StdinPipe(); err != nil {
		p.cleanup()
		return nil, err
	}
	if err := p.cmd.Start(); err != nil {
		p.cleanup()
		return nil, fmt.Errorf("target %s: %w", p.name, err)
	}
	p.exited = make(chan error, 1)
	go func() { p.exited <- p.cmd.Wait() }()
	auth := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(metadata.AppendToOutgoingContext(ctx, TokenKey, token), method, req, reply, cc, opts...)
	}
	authStream := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(metadata.AppendToOutgoingContext(ctx, TokenKey, token), desc, cc, method, opts...)
	}
	// the provider is starting: try often, not after gRPC's usual second
	connect := grpc.ConnectParams{Backoff: backoff.Config{BaseDelay: 20 * time.Millisecond, Multiplier: 1.5, MaxDelay: time.Second}, MinConnectTimeout: time.Second}
	if p.conn, err = grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithConnectParams(connect),
		grpc.WithUnaryInterceptor(auth), grpc.WithStreamInterceptor(authStream)); err != nil {
		p.Close()
		return nil, err
	}
	p.client = providerpb.NewProviderClient(p.conn)
	deadline := time.Now().Add(within)
	var hs *providerpb.HandshakeResponse
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		hs, err = p.client.Handshake(ctx, &providerpb.HandshakeRequest{Protocol: Protocol}, grpc.WaitForReady(true))
		cancel()
		if err == nil {
			break
		}
		if s, ok := status.FromError(err); ok && s.Code() != codes.Unavailable && s.Code() != codes.DeadlineExceeded {
			p.Close()
			return nil, fmt.Errorf("target %s: handshake: %w", p.name, unwrap(err))
		}
		select {
		case werr := <-p.exited:
			p.exited <- werr
			p.Close()
			return nil, fmt.Errorf("target %s: %s exited before answering: %v", p.name, p.what, werr)
		default:
		}
		if time.Now().After(deadline) {
			p.Close()
			return nil, fmt.Errorf("target %s: %s did not answer within %s: %v", p.name, p.what, within, unwrap(err))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hs.Protocol != Protocol || hs.Target != p.name {
		p.Close()
		return nil, fmt.Errorf("target %s: %s is target %q speaking %s, wanted target %q speaking %s", p.name, p.what, hs.Target, hs.Protocol, p.name, Protocol)
	}
	p.caps = hs.Capabilities
	return p, nil
}

func (p *Plugin) cleanup() {
	if len(p.dir) > 0 {
		_ = os.RemoveAll(p.dir)
	}
}

func containerRuntime() (string, error) {
	if r := os.Getenv(EnvRuntime); len(r) > 0 {
		return exec.LookPath(r)
	}
	for _, r := range []string{"podman", "docker"} {
		if p, err := exec.LookPath(r); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("a provider image needs a container runtime: no podman or docker on PATH (%s names another)", EnvRuntime)
}

func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	return port, err
}

func newToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Close ends the provider: its stdin closes, it stops serving and exits.
func (p *Plugin) Close() {
	if p.conn != nil {
		_ = p.conn.Close()
	}
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.exited != nil {
		select {
		case <-p.exited:
		case <-time.After(5 * time.Second):
			if p.kill != nil {
				p.kill()
			}
			_ = p.cmd.Process.Kill()
			<-p.exited
		}
	}
	p.cleanup()
}

func (p *Plugin) Name() string { return p.name }

// Capabilities are what the provider declared in the handshake.
func (p *Plugin) Capabilities() []string { return p.caps }

func (p *Plugin) Configure(installation string, settings target.Settings) (target.Installation, error) {
	resp, err := p.client.Configure(context.Background(), &providerpb.ConfigureRequest{Installation: installation, Settings: settings})
	if err != nil {
		return nil, unwrap(err)
	}
	r := &remote{p: p, namespace: resp.Namespace, schema: resp.PlatformSchema}
	if slices.Contains(p.caps, target.CapExecute) {
		return &executing{r}, nil
	}
	return r, nil
}

// executing is an installation of a provider that declared "execute".
type executing struct{ *remote }

func (x *executing) ReadInstalled() ([]byte, bool, error) {
	resp, err := x.p.client.ReadInstalled(context.Background(), &providerpb.ReadInstalledRequest{})
	if err != nil {
		return nil, false, unwrap(err)
	}
	return resp.Record, resp.Exists, nil
}

func (x *executing) WriteInstalled(record []byte) error {
	_, err := x.p.client.WriteInstalled(context.Background(), &providerpb.WriteInstalledRequest{Record: record})
	return unwrap(err)
}

func (x *executing) Execute(ctx context.Context, step target.Step, events func(string)) error {
	req, err := stepTo(step)
	if err != nil {
		return err
	}
	stream, err := x.p.client.Execute(ctx, req)
	if err != nil {
		return unwrap(err)
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("target %s: step %d ended without a result", x.p.name, step.N)
			}
			return unwrap(err)
		}
		if len(ev.Message) > 0 && events != nil {
			events(ev.Message)
		}
		if ev.Done {
			if len(ev.Error) > 0 {
				return errors.New(ev.Error)
			}
			return nil
		}
	}
}

// remote is the provider bound to an installation, as a target.Installation.
type remote struct {
	p         *Plugin
	namespace string
	schema    []byte
}

func (r *remote) Namespace() string      { return r.namespace }
func (r *remote) PlatformSchema() []byte { return r.schema }

func (r *remote) Resolve(key, scale string, platform map[string]any, installed bool) (*render.Resolved, error) {
	raw, err := encode(platform)
	if err != nil {
		return nil, err
	}
	resp, err := r.p.client.Resolve(context.Background(), &providerpb.ResolveRequest{Key: key, Scale: scale, PlatformJson: raw, Installed: installed})
	if err != nil {
		return nil, unwrap(err)
	}
	res := &render.Resolved{Service: resp.Service, Address: resp.Address}
	for _, in := range resp.Instances {
		res.Instances = append(res.Instances, render.Instance{Name: in.Name, Address: in.Address})
	}
	if res.Platform, err = decodeMap(resp.PlatformJson); err != nil {
		return nil, err
	}
	return res, nil
}

func (r *remote) ValidatePlatform(key string, platform map[string]any) []string {
	raw, err := encode(platform)
	if err != nil {
		return []string{fmt.Sprintf("/%s/platform: %v", key, err)}
	}
	resp, err := r.p.client.ValidatePlatform(context.Background(), &providerpb.ValidatePlatformRequest{Key: key, PlatformJson: raw})
	if err != nil {
		return []string{fmt.Sprintf("/%s/platform: target %s: %v", key, r.p.name, unwrap(err))}
	}
	return resp.Problems
}

func (r *remote) Convert(rel *render.Release, credentials []render.Credential) (*target.Output, error) {
	prel, err := releaseTo(rel)
	if err != nil {
		return nil, err
	}
	resp, err := r.p.client.Convert(context.Background(), &providerpb.ConvertRequest{Release: prel, Credentials: credentialsTo(credentials)})
	if err != nil {
		return nil, unwrap(err)
	}
	out := &target.Output{}
	for _, f := range resp.Files {
		out.Files = append(out.Files, target.File{Path: f.Path, Data: bytes.Clone(f.Data), Mode: fs.FileMode(f.Mode), Stream: f.Stream})
	}
	return out, nil
}

// unwrap turns a gRPC status into the provider's own message.
func unwrap(err error) error {
	if err == nil {
		return nil
	}
	if s, ok := status.FromError(err); ok {
		return errors.New(strings.TrimSpace(s.Message()))
	}
	return err
}

var (
	_ target.Target       = (*Plugin)(nil)
	_ target.Capable      = (*Plugin)(nil)
	_ target.Installation = (*remote)(nil)
	_ target.Executor     = (*executing)(nil)
)
