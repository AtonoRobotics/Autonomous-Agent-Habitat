package grpcapi

import (
	"context"
	"log/slog"
	"net"

	"google.golang.org/grpc"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/grpcapi/policypb"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/policy"
)

// Server is the gRPC counterpart of daemon/api.Server — one more
// supervised child of amh-daemon, alongside the HTTP api server it runs
// next to (see this package's doc comment for the split). Phase 1 wires
// only PolicyService; later phases add operations/selfimprove/inference
// services to the same *grpc.Server as they migrate.
type Server struct {
	Addr   string
	Policy *policy.Engine
	Auth   *authn.Authenticator
	Log    *slog.Logger

	srv *grpc.Server
}

func New(addr string, pol *policy.Engine, auth *authn.Authenticator, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{Addr: addr, Policy: pol, Auth: auth, Log: log}
}

// Run blocks, serving the gRPC API, until ctx is cancelled. Matches
// supervisor.Child.Run's signature — see daemon/api.Server.Run, which
// this mirrors for the HTTP/2-based transport instead of HTTP/1.1+JSON.
func (s *Server) Run(ctx context.Context) error {
	roles := policyRoles()
	grpcSrv := grpc.NewServer(grpc.UnaryInterceptor(authInterceptor(s.Auth, roles)))
	policypb.RegisterPolicyServiceServer(grpcSrv, &policyServer{Policy: s.Policy})
	s.srv = grpcSrv

	lis, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		s.Log.Info("grpcapi: listening", "addr", s.Addr)
		if err := grpcSrv.Serve(lis); err != nil {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		// GracefulStop drains in-flight RPCs rather than dropping them —
		// the same "give it a chance to finish" posture api.Server.Run's
		// http.Server.Shutdown already takes, just gRPC's own mechanism
		// for it rather than a manual timeout context.
		grpcSrv.GracefulStop()
		return nil
	case err := <-errCh:
		return err
	}
}
