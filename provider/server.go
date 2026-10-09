// Package provider runs a target in its own process. A provider binary
// (pack-provider-<target>) calls Serve with its target.Target; pack calls
// Launch, which starts the binary and gives back a target.Target that asks
// it over gRPC (proto/provider.proto). Neither side links the other: pack
// knows no target, a provider knows the render's types and nothing of the CLI.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ks-tool/pack/provider/providerpb"
	"github.com/ks-tool/pack/target"
)

const (
	// Protocol is the version of the provider protocol this package speaks.
	Protocol = "pack.provider.v1"
	// EnvSocket is where pack tells the provider to listen.
	EnvSocket = "PACK_PROVIDER_SOCKET"
)

// Serve is the whole main of a provider binary: it listens on the socket
// pack gave it, serves t, and returns when pack closes its stdin.
func Serve(t target.Target) error {
	sock := os.Getenv(EnvSocket)
	if len(sock) == 0 {
		return fmt.Errorf("%s is not set: this is a pack provider (target %s), pack starts it", EnvSocket, t.Name())
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	s := grpc.NewServer()
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
	inst, err := s.t.Configure(req.Installation, target.Settings(req.Settings))
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
	res, err := inst.Resolve(req.Key, req.Scale, platform)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	out := &providerpb.ResolveResponse{Service: res.Service, Address: res.Address}
	for _, in := range res.Instances {
		out.Instances = append(out.Instances, &providerpb.Instance{Name: in.Name, Address: in.Address})
	}
	if out.LimitsJson, err = encode(res.Limits); err != nil {
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
	return &providerpb.ValidatePlatformResponse{Problems: inst.ValidatePlatform(req.Key, platform, req.Minimum)}, nil
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
