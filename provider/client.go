package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/ks-tool/pack/provider/providerpb"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

const (
	// EnvPath lists directories searched for provider binaries before the
	// directory of pack itself and PATH.
	EnvPath = "PACK_PROVIDER_PATH"
	// Prefix names a provider binary: pack-provider-<target>.
	Prefix = "pack-provider-"

	startTimeout = 10 * time.Second
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
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	exited chan error // the process's one Wait
	dir    string
	conn   *grpc.ClientConn
	client providerpb.ProviderClient
	caps   []string
}

// Launch finds and starts the provider of a target and shakes hands with it.
func Launch(name string) (*Plugin, error) {
	bin, err := Find(name)
	if err != nil {
		return nil, err
	}
	return Start(name, bin)
}

// Start runs the provider binary at bin for the target name.
func Start(name, bin string) (*Plugin, error) {
	dir, err := os.MkdirTemp("", "pack-provider-")
	if err != nil {
		return nil, err
	}
	p := &Plugin{name: name, dir: dir}
	sock := filepath.Join(dir, "provider.sock")
	p.cmd = exec.Command(bin)
	p.cmd.Env = append(os.Environ(), EnvSocket+"="+sock)
	p.cmd.Stdout = os.Stderr // a provider has nothing to say on pack's stdout
	p.cmd.Stderr = os.Stderr
	if p.stdin, err = p.cmd.StdinPipe(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := p.cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("target %s: %w", name, err)
	}
	p.exited = make(chan error, 1)
	go func() { p.exited <- p.cmd.Wait() }()
	deadline := time.Now().Add(startTimeout)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		select {
		case err := <-p.exited:
			p.exited <- err
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("target %s: %s exited before listening: %v", name, bin, err)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			p.Close()
			return nil, fmt.Errorf("target %s: %s did not listen within %s", name, bin, startTimeout)
		}
	}
	if p.conn, err = grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials())); err != nil {
		p.Close()
		return nil, err
	}
	p.client = providerpb.NewProviderClient(p.conn)
	hs, err := p.client.Handshake(context.Background(), &providerpb.HandshakeRequest{Protocol: Protocol})
	if err != nil {
		p.Close()
		return nil, fmt.Errorf("target %s: handshake: %w", name, unwrap(err))
	}
	if hs.Protocol != Protocol || hs.Target != name {
		p.Close()
		return nil, fmt.Errorf("target %s: %s is target %q speaking %s, wanted target %q speaking %s", name, bin, hs.Target, hs.Protocol, name, Protocol)
	}
	p.caps = hs.Capabilities
	return p, nil
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
			_ = p.cmd.Process.Kill()
			<-p.exited
		}
	}
	_ = os.RemoveAll(p.dir)
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

func (r *remote) Resolve(key, scale string, platform map[string]any) (*render.Resolved, error) {
	raw, err := encode(platform)
	if err != nil {
		return nil, err
	}
	resp, err := r.p.client.Resolve(context.Background(), &providerpb.ResolveRequest{Key: key, Scale: scale, PlatformJson: raw})
	if err != nil {
		return nil, unwrap(err)
	}
	res := &render.Resolved{Service: resp.Service, Address: resp.Address}
	for _, in := range resp.Instances {
		res.Instances = append(res.Instances, render.Instance{Name: in.Name, Address: in.Address})
	}
	if res.Limits, err = decodeMap(resp.LimitsJson); err != nil {
		return nil, err
	}
	return res, nil
}

func (r *remote) ValidatePlatform(key string, platform map[string]any, minimum map[string]string) []string {
	raw, err := encode(platform)
	if err != nil {
		return []string{fmt.Sprintf("/%s/platform: %v", key, err)}
	}
	resp, err := r.p.client.ValidatePlatform(context.Background(), &providerpb.ValidatePlatformRequest{Key: key, PlatformJson: raw, Minimum: minimum})
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
