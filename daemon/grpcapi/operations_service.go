package grpcapi

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/extensions"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/grpcapi/operationspb"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/operations"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/policy"
)

// operationsServer implements operationspb.OperationsServiceServer over
// daemon/operations.Engine — the same §4 external-effect lifecycle
// daemon/api/operations.go already exposes over HTTP, real business logic
// shared between both transports, only the marshaling and effect-ownership
// authorization plumbing differ.
type operationsServer struct {
	operationspb.UnimplementedOperationsServiceServer
	Operations *operations.Engine
	Extensions *extensions.Registry
}

// operationsRoles is this service's method allow-list. Every method is
// agent-or-operator (the "agents propose" half of decision 9, and every
// transition past Propose is the caller mechanically reporting real
// dispatch progress it already has authority to perform) — the same
// blanket posture daemon/api/operations.go's own doc comment states for
// its HTTP routes. Fine-grained effect-ownership authorization (§15
// invariant #5) is a separate, per-effect check each method runs itself
// (see authorizeEffectOwner/authorizeEffectMutation below), not something
// a per-route role allow-list can express.
func operationsRoles() requiredRoles {
	full := "/amh.operations.v1.OperationsService/"
	agentOrOperator := []authn.Role{authn.RoleAgent, authn.RoleOperator}
	return requiredRoles{
		full + "Propose":                agentOrOperator,
		full + "GetEffect":              agentOrOperator,
		full + "ListEffectsByOperation": agentOrOperator,
		full + "MarkDispatchPending":    agentOrOperator,
		full + "MarkDispatched":         agentOrOperator,
		full + "MarkObserved":           agentOrOperator,
		full + "MarkOutcomeUnknown":     agentOrOperator,
		full + "Resolve":                agentOrOperator,
	}
}

// operationsCorePrefix marks an owner_extension_id as core-owned, not a
// genuine third-party extension — see daemon/api/operations.go's
// corePrefix, which this mirrors exactly (every real core call site
// self-labels this way and either never reaches either transport's HTTP/
// gRPC layer at all, or is itself core-owned Python code).
const operationsCorePrefix = "amh.core/"

// extensionTokenFromContext reads the gRPC metadata analog of the HTTP
// client's X-AMH-Extension-Token header. gRPC lowercases metadata keys.
func extensionTokenFromContext(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	values := md.Get("x-amh-extension-token")
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// authorizeEffectOwner enforces §15 acceptance invariant #5, the gRPC
// mirror of daemon/api/operations.go's authorizeEffectOwner: an operator
// always passes, a core-owned ownerExtensionID needs nothing beyond the
// ordinary agent/operator role check authInterceptor already ran, and
// anything else requires the caller's own capability token to name that
// exact extension.
func (s *operationsServer) authorizeEffectOwner(ctx context.Context, ownerExtensionID string) error {
	if role, ok := RoleFromContext(ctx); ok && role == authn.RoleOperator {
		return nil
	}
	if strings.HasPrefix(ownerExtensionID, operationsCorePrefix) {
		return nil
	}
	token := extensionTokenFromContext(ctx)
	extID, _, ok, err := s.Extensions.VerifyCapabilityToken(ctx, token)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if !ok || extID != ownerExtensionID {
		return status.Error(codes.PermissionDenied, "grpcapi: caller is not authorized to act on behalf of owner_extension_id "+ownerExtensionID)
	}
	return nil
}

// authorizeEffectMutation loads effectID and authorizes the caller
// against its owner_extension_id in one step — every mutating method past
// Propose needs this, since (unlike Propose) the owner isn't in their own
// request.
func (s *operationsServer) authorizeEffectMutation(ctx context.Context, effectID string) (*operations.Effect, error) {
	eff, err := s.Operations.Get(ctx, effectID)
	if err != nil {
		return nil, operationsErrorStatus(err)
	}
	if err := s.authorizeEffectOwner(ctx, eff.OwnerExtensionID); err != nil {
		return nil, err
	}
	return eff, nil
}

func operationsErrorStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case status.Code(err) != codes.Unknown:
		return err
	case errors.Is(err, operations.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, operations.ErrInvalidTransition), errors.Is(err, policy.ErrAlreadyConsumed),
		errors.Is(err, policy.ErrDigestMismatch), errors.Is(err, policy.ErrDecisionExpired), errors.Is(err, policy.ErrNotAdmitted):
		// Same "no direct 409 analog" reasoning as policyErrorStatus.
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.InvalidArgument, err.Error())
	}
}

func toEffectPB(eff *operations.Effect) *operationspb.Effect {
	return &operationspb.Effect{
		EffectId: eff.EffectID, OperationId: eff.OperationID, OwnerExtensionId: eff.OwnerExtensionID,
		EffectType: eff.EffectType, DecisionId: eff.DecisionID, State: string(eff.State),
		ForwardDigest: eff.ForwardDigest, RetryClass: string(eff.RetryClass),
		ExternalCommandId: eff.ExternalCommandID, ObservationRef: eff.ObservationRef, ObservationPayload: eff.ObservationPayload,
		ErrorCode: eff.ErrorCode, ErrorRetryable: eff.ErrorRetryable, ErrorMessage: eff.ErrorMessage,
		CreatedAt: eff.CreatedAt, UpdatedAt: eff.UpdatedAt,
	}
}

func (s *operationsServer) Propose(ctx context.Context, req *operationspb.ProposeRequest) (*operationspb.Effect, error) {
	if err := s.authorizeEffectOwner(ctx, req.OwnerExtensionId); err != nil {
		return nil, err
	}
	payload, err := unmarshalPayload(req.PayloadJson)
	if err != nil {
		return nil, err
	}
	eff, err := s.Operations.Propose(ctx, operations.ProposeRequest{
		OperationID: req.OperationId, OwnerExtensionID: req.OwnerExtensionId, EffectType: req.EffectType,
		Payload: payload, Reversibility: policy.Reversibility(req.Reversibility),
		RetryClass: operations.RetryClass(req.RetryClass),
	})
	if err != nil {
		return nil, operationsErrorStatus(err)
	}
	return toEffectPB(eff), nil
}

func (s *operationsServer) GetEffect(ctx context.Context, req *operationspb.GetEffectRequest) (*operationspb.Effect, error) {
	eff, err := s.Operations.Get(ctx, req.EffectId)
	if err != nil {
		return nil, operationsErrorStatus(err)
	}
	return toEffectPB(eff), nil
}

func (s *operationsServer) ListEffectsByOperation(ctx context.Context, req *operationspb.ListEffectsByOperationRequest) (*operationspb.ListEffectsByOperationResponse, error) {
	if req.OperationId == "" {
		return nil, status.Error(codes.InvalidArgument, "operation_id is required")
	}
	list, err := s.Operations.ListByOperation(ctx, req.OperationId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*operationspb.Effect, 0, len(list))
	for _, eff := range list {
		out = append(out, toEffectPB(eff))
	}
	return &operationspb.ListEffectsByOperationResponse{Effects: out}, nil
}

func (s *operationsServer) MarkDispatchPending(ctx context.Context, req *operationspb.MarkDispatchPendingRequest) (*operationspb.Effect, error) {
	if _, err := s.authorizeEffectMutation(ctx, req.EffectId); err != nil {
		return nil, err
	}
	payload, err := unmarshalPayload(req.PayloadJson)
	if err != nil {
		return nil, err
	}
	eff, err := s.Operations.MarkDispatchPending(ctx, req.EffectId, payload)
	if err != nil {
		return nil, operationsErrorStatus(err)
	}
	return toEffectPB(eff), nil
}

func (s *operationsServer) MarkDispatched(ctx context.Context, req *operationspb.MarkDispatchedRequest) (*operationspb.Effect, error) {
	if _, err := s.authorizeEffectMutation(ctx, req.EffectId); err != nil {
		return nil, err
	}
	eff, err := s.Operations.MarkDispatched(ctx, req.EffectId, req.ExternalCommandId)
	if err != nil {
		return nil, operationsErrorStatus(err)
	}
	return toEffectPB(eff), nil
}

func (s *operationsServer) MarkObserved(ctx context.Context, req *operationspb.MarkObservedRequest) (*operationspb.Effect, error) {
	if _, err := s.authorizeEffectMutation(ctx, req.EffectId); err != nil {
		return nil, err
	}
	eff, err := s.Operations.MarkObserved(ctx, req.EffectId, req.ObservationRef, req.ObservationPayload)
	if err != nil {
		return nil, operationsErrorStatus(err)
	}
	return toEffectPB(eff), nil
}

func (s *operationsServer) MarkOutcomeUnknown(ctx context.Context, req *operationspb.MarkOutcomeUnknownRequest) (*operationspb.Effect, error) {
	if _, err := s.authorizeEffectMutation(ctx, req.EffectId); err != nil {
		return nil, err
	}
	eff, err := s.Operations.MarkOutcomeUnknown(ctx, req.EffectId)
	if err != nil {
		return nil, operationsErrorStatus(err)
	}
	return toEffectPB(eff), nil
}

func (s *operationsServer) Resolve(ctx context.Context, req *operationspb.ResolveRequest) (*operationspb.Effect, error) {
	if _, err := s.authorizeEffectMutation(ctx, req.EffectId); err != nil {
		return nil, err
	}
	var effErr *operations.EffectError
	if req.ErrorCode != "" || req.Message != "" {
		effErr = &operations.EffectError{Code: req.ErrorCode, Retryable: req.Retryable, Message: req.Message}
	}
	eff, err := s.Operations.Resolve(ctx, req.EffectId, operations.State(req.Terminal), effErr)
	if err != nil {
		return nil, operationsErrorStatus(err)
	}
	return toEffectPB(eff), nil
}
