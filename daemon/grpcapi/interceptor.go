// Package grpcapi is the daemon-resident gRPC surface for the internal,
// purely synchronous Go-daemon <-> Python-cognition-worker call path
// (docs/AMH-SPECIFICATION.md §3.1/§3.3: "local gRPC transport" / "local
// gRPC for synchronous daemon/worker calls"). It exists alongside
// daemon/api's HTTP surface, not instead of it — see this package's own
// migration doc comment in contracts/proto/policy.proto for exactly
// which routes move here and which stay HTTP (anything the control-plane
// UI's browser JS calls directly, or that speaks an external protocol
// like A2A/MCP/OpenAI-compatible, has no reason to and does not move).
package grpcapi

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
)

type roleContextKey struct{}

// RoleFromContext returns the role authInterceptor authenticated this
// call as. Mirrors daemon/authn.RoleFromContext's HTTP-side contract
// exactly, so a handler that needs the fine-grained "is this specifically
// an operator" check (the same pattern daemon/api/operations.go's
// authorizeEffectOwner already uses over HTTP) can ask the identical
// question over gRPC.
func RoleFromContext(ctx context.Context) (authn.Role, bool) {
	role, ok := ctx.Value(roleContextKey{}).(authn.Role)
	return role, ok
}

// requiredRoles maps a fully-qualified gRPC method name to the roles
// allowed to call it — the gRPC equivalent of daemon/api's route table,
// serving the same purpose RequireRole's per-route allow-list does over
// HTTP: read this map, not scattered per-handler checks, for "who can
// call what."
type requiredRoles map[string][]authn.Role

// authInterceptor authenticates every unary call the same way
// daemon/authn.RequireRole does over HTTP: a missing/malformed bearer
// token or an unrecognized one is Unauthenticated, a recognized token
// whose role isn't in this method's allow-list is PermissionDenied —
// distinguishing "who are you" from "you can't do that," the same
// distinction RequireRole draws via 401 vs 403. Reuses
// authn.Authenticator.Authenticate (== roleFor) rather than a second
// token-comparison implementation, so the two transports can never
// silently drift on what counts as a valid token.
func authInterceptor(auth *authn.Authenticator, allowed requiredRoles) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "authn: missing metadata")
		}
		values := md.Get("authorization")
		if len(values) == 0 {
			return nil, status.Error(codes.Unauthenticated, authn.ErrMissingAuthHeader.Error())
		}
		const prefix = "Bearer "
		header := values[0]
		if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
			return nil, status.Error(codes.Unauthenticated, authn.ErrMissingAuthHeader.Error())
		}
		token := header[len(prefix):]
		role, ok := auth.Authenticate(token)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, authn.ErrInvalidToken.Error())
		}
		roles, known := allowed[info.FullMethod]
		if !known {
			// Fail closed: a method with no explicit entry in the allow-list
			// is refused, not silently open to any authenticated caller —
			// the same "every route lists every role it accepts" discipline
			// daemon/authn's own doc comment requires over HTTP.
			return nil, status.Errorf(codes.PermissionDenied, "grpcapi: %s has no configured role allow-list", info.FullMethod)
		}
		permitted := false
		for _, r := range roles {
			if r == role {
				permitted = true
				break
			}
		}
		if !permitted {
			return nil, status.Error(codes.PermissionDenied, authn.ErrRoleNotAllowed.Error())
		}
		return handler(context.WithValue(ctx, roleContextKey{}, role), req)
	}
}
