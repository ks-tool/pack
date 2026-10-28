// Package provider runs a target in its own process — a binary, or a
// container image when the provider is written in a language whose runtime
// the machine running pack does not have. A provider written in Go calls
// Serve with its target.Target; pack calls Launch, which starts the binary
// or the image and gives back a target.Target that asks it over gRPC
// (proto/provider.proto). Neither side links the other: pack knows no
// target, a provider knows the render's types and nothing of the CLI.
//
// What a provider in any language is told, in its environment:
//
//	PACK_PROVIDER_ADDRESS  where to listen: unix:///<path> (a binary) or tcp://<host>:<port> (an image)
//	PACK_PROVIDER_TOKEN    a token every call carries as the metadata pack-provider-token; refuse a call without it
//
// and EOF on stdin means pack is done with it: exit.
package provider

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/ks-tool/pack/provider/providerpb"
	"github.com/ks-tool/pack/target"
)

const (
	// Protocol is the version of the provider protocol this package speaks.
	Protocol = "pack.provider.v1"
	// EnvAddress is where pack tells the provider to listen.
	EnvAddress = "PACK_PROVIDER_ADDRESS"
	// EnvToken is the token every call of pack carries.
	EnvToken = "PACK_PROVIDER_TOKEN"
	// TokenKey is the metadata key the token travels under.
	TokenKey = "pack-provider-token"
)

// MaxMessage bounds one message between pack and a provider: a release
// carries the files of the pack — an archive of data among them — far
// beyond gRPC's default 4 MiB.
const MaxMessage = 2<<30 - 1

// Serve is the whole main of a provider binary: it listens where pack told
// it, serves t to whoever has the token, and returns when pack closes its
// stdin.
func Serve(t target.Target) error {
	addr := os.Getenv(EnvAddress)
	if len(addr) == 0 {
		return fmt.Errorf("%s is not set: this is a pack provider (target %s), pack starts it", EnvAddress, t.Name())
	}
	token := os.Getenv(EnvToken)
	if len(token) == 0 {
		return fmt.Errorf("%s is not set: pack gives every provider a token", EnvToken)
	}
	var l net.Listener
	var err error
	switch {
	case strings.HasPrefix(addr, "unix://"):
		l, err = net.Listen("unix", strings.TrimPrefix(addr, "unix://"))
	case strings.HasPrefix(addr, "tcp://"):
		l, err = net.Listen("tcp", strings.TrimPrefix(addr, "tcp://"))
	default:
		return fmt.Errorf("%s %q: unix:///<path> or tcp://<host>:<port>", EnvAddress, addr)
	}
	if err != nil {
		return err
	}
	s := grpc.NewServer(grpc.MaxRecvMsgSize(MaxMessage), grpc.MaxSendMsgSize(MaxMessage), grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if err := authorized(ctx, token); err != nil {
			return nil, err
		}
		return h(ctx, req)
	}), grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		if err := authorized(ss.Context(), token); err != nil {
			return err
		}
		return h(srv, ss)
	}))
	providerpb.RegisterProviderServer(s, &server{t: t})
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	go func() {
		// pack holds our stdin; EOF means pack is gone or done with us.
		_, _ = io.Copy(io.Discard, os.Stdin)
		s.GracefulStop()
	}()
	if err := <-done; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

// authorized: the call carries the token pack gave this provider. A provider
// on TCP is reachable by anyone on the machine; the token is what makes it pack's.
func authorized(ctx context.Context, token string) error {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get(TokenKey) {
		if subtle.ConstantTimeCompare([]byte(v), []byte(token)) == 1 {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "no or wrong "+TokenKey)
}

// server answers for one target, bound to one installation by Configure.
type server struct {
	providerpb.UnimplementedProviderServer
	t target.Target

	mu   sync.Mutex
	inst target.Installation
}

func (s *server) installation() (target.Installation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inst == nil {
		return nil, status.Error(codes.FailedPrecondition, "Configure first")
	}
	return s.inst, nil
}

func (s *server) Handshake(_ context.Context, req *providerpb.HandshakeRequest) (*providerpb.HandshakeResponse, error) {
	if req.Protocol != Protocol {
		return nil, status.Errorf(codes.FailedPrecondition, "protocol %q: this provider speaks %s", req.Protocol, Protocol)
	}
	resp := &providerpb.HandshakeResponse{Protocol: Protocol, Target: s.t.Name()}
	if c, ok := s.t.(target.Capable); ok {
		resp.Capabilities = c.Capabilities()
	}
	return resp, nil
}

func (s *server) Configure(_ context.Context, req *providerpb.ConfigureRequest) (*providerpb.ConfigureResponse, error) {
	inst, err := s.t.Configure(req.Installation, req.Settings)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s.mu.Lock()
	s.inst = inst
	s.mu.Unlock()
	return &providerpb.ConfigureResponse{Namespace: inst.Namespace(), PlatformSchema: inst.PlatformSchema()}, nil
}

func (s *server) Resolve(_ context.Context, req *providerpb.ResolveRequest) (*providerpb.ResolveResponse, error) {
	inst, err := s.installation()
	if err != nil {
		return nil, err
	}
	platform, err := decodeMap(req.PlatformJson)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	res, err := inst.Resolve(req.Key, req.Scale, platform, req.Installed)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	out := &providerpb.ResolveResponse{Service: res.Service, Address: res.Address}
	for _, in := range res.Instances {
		out.Instances = append(out.Instances, &providerpb.Instance{Name: in.Name, Address: in.Address})
	}
	if out.PlatformJson, err = encode(res.Platform); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return out, nil
}

func (s *server) ValidatePlatform(_ context.Context, req *providerpb.ValidatePlatformRequest) (*providerpb.ValidatePlatformResponse, error) {
	inst, err := s.installation()
	if err != nil {
		return nil, err
	}
	platform, err := decodeMap(req.PlatformJson)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &providerpb.ValidatePlatformResponse{Problems: inst.ValidatePlatform(req.Key, platform)}, nil
}

func (s *server) Convert(_ context.Context, req *providerpb.ConvertRequest) (*providerpb.ConvertResponse, error) {
	inst, err := s.installation()
	if err != nil {
		return nil, err
	}
	rel, err := releaseFrom(req.Release)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	out, err := inst.Convert(rel, credentialsFrom(req.Credentials))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	resp := &providerpb.ConvertResponse{}
	for _, f := range out.Files {
		resp.Files = append(resp.Files, &providerpb.File{Path: f.Path, Data: f.Data, Mode: uint32(f.Mode), Stream: f.Stream})
	}
	return resp, nil
}

func (s *server) executor() (target.Executor, error) {
	inst, err := s.installation()
	if err != nil {
		return nil, err
	}
	x, ok := inst.(target.Executor)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "target %s does not execute steps", s.t.Name())
	}
	return x, nil
}

func (s *server) ReadInstalled(context.Context, *providerpb.ReadInstalledRequest) (*providerpb.ReadInstalledResponse, error) {
	x, err := s.executor()
	if err != nil {
		return nil, err
	}
	rec, exists, err := x.ReadInstalled()
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return &providerpb.ReadInstalledResponse{Exists: exists, Record: rec}, nil
}

func (s *server) WriteInstalled(_ context.Context, req *providerpb.WriteInstalledRequest) (*providerpb.WriteInstalledResponse, error) {
	x, err := s.executor()
	if err != nil {
		return nil, err
	}
	if err := x.WriteInstalled(req.Record); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return &providerpb.WriteInstalledResponse{}, nil
}

func (s *server) Execute(req *providerpb.ExecuteRequest, stream providerpb.Provider_ExecuteServer) error {
	x, err := s.executor()
	if err != nil {
		return err
	}
	step, err := stepFrom(req)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	var sendErr error
	events := func(msg string) {
		if sendErr == nil {
			sendErr = stream.Send(&providerpb.ExecuteEvent{Message: msg})
		}
	}
	last := &providerpb.ExecuteEvent{Done: true}
	if err := x.Execute(stream.Context(), step, events); err != nil {
		last.Error = err.Error()
		if len(last.Error) == 0 {
			last.Error = "failed"
		}
	}
	if sendErr != nil {
		return sendErr
	}
	return stream.Send(last)
}
