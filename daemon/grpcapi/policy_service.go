package grpcapi

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/grpcapi/policypb"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/policy"
)

// policyServer implements policypb.PolicyServiceServer over
// daemon/policy.Engine — the exact same generic fail-closed policy/
// approval seam daemon/api/policy.go already exposes over HTTP, real
// business logic shared between both transports, only the marshaling
// differs.
type policyServer struct {
	policypb.UnimplementedPolicyServiceServer
	Policy *policy.Engine
}

// policyRoles is this service's method allow-list, the gRPC mirror of
// api.go's route table: Decide/Consume/GetDecision/ListPendingApprovals/
// GetApprovalRequest are agent-or-operator (decision 9: "agents
// propose"); Approve/Deny are operator-only, the same anti-self-approval
// property daemon/authn's own doc comment describes.
func policyRoles() requiredRoles {
	full := "/amh.policy.v1.PolicyService/"
	return requiredRoles{
		full + "Decide":               {authn.RoleAgent, authn.RoleOperator},
		full + "GetDecision":          {authn.RoleAgent, authn.RoleOperator},
		full + "Consume":              {authn.RoleAgent, authn.RoleOperator},
		full + "ListPendingApprovals": {authn.RoleAgent, authn.RoleOperator},
		full + "GetApprovalRequest":   {authn.RoleAgent, authn.RoleOperator},
		full + "Approve":              {authn.RoleOperator},
		full + "Deny":                 {authn.RoleOperator},
	}
}

func policyErrorStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case status.Code(err) != codes.Unknown:
		// Already a real gRPC status (e.g. from a handler that
		// constructed one directly) — pass it through unwrapped.
		return err
	default:
	}
	switch {
	case errors.Is(err, policy.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, policy.ErrAlreadyConsumed), errors.Is(err, policy.ErrNotAdmitted),
		errors.Is(err, policy.ErrDigestMismatch), errors.Is(err, policy.ErrDecisionExpired),
		errors.Is(err, policy.ErrNotPending):
		// gRPC has no direct "409 Conflict" analog; FailedPrecondition is
		// the documented code for "the system is not in a state required
		// for the operation's execution" — the same fail-closed-on-
		// mutation property invariant #6 requires, just a different wire
		// encoding of the same refusal daemon/api/policy.go's
		// policyErrorStatus already returns as 409 over HTTP.
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.InvalidArgument, err.Error())
	}
}

func toDecisionPB(d *policy.Decision) *policypb.Decision {
	return &policypb.Decision{
		Id: d.ID, OperationId: d.OperationID, ActionDigest: d.ActionDigest,
		PolicyId: d.PolicyID, PolicyVersion: d.PolicyVersion, Result: string(d.Result),
		ReasonCodes: d.ReasonCodes, ApprovalRequestId: d.ApprovalRequestID,
		DecidedAt: d.DecidedAt, ExpiresAt: d.ExpiresAt, ConsumedAt: d.ConsumedAt,
	}
}

func toApprovalRequestPB(a *policy.ApprovalRequest) *policypb.ApprovalRequest {
	return &policypb.ApprovalRequest{
		Id: a.ID, DecisionId: a.DecisionID, Status: a.Status,
		ResolvedBy: a.ResolvedBy, ResolvedAt: a.ResolvedAt, Reason: a.Reason,
	}
}

// unmarshalPayload decodes a request's payload_json field the same way
// daemon/api/policy.go's handlers decode the "payload" field embedded in
// a JSON request body — same json.Unmarshal, same resulting Go value
// shape (map[string]any / []any / scalars), so policy.Digest computes
// byte-identical digests regardless of which transport carried the call.
// An empty string decodes to nil, matching an HTTP caller that omits the
// field entirely.
func unmarshalPayload(payloadJSON string) (any, error) {
	if payloadJSON == "" {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal([]byte(payloadJSON), &v); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "grpcapi: invalid payload_json: %v", err)
	}
	return v, nil
}

func (s *policyServer) Decide(ctx context.Context, req *policypb.DecideRequest) (*policypb.Decision, error) {
	if req.OperationId == "" {
		return nil, status.Error(codes.InvalidArgument, "operation_id is required")
	}
	payload, err := unmarshalPayload(req.PayloadJson)
	if err != nil {
		return nil, err
	}
	d, err := s.Policy.Decide(ctx, policy.DecideRequest{
		OperationID:   req.OperationId,
		Payload:       payload,
		Reversibility: policy.Reversibility(req.Reversibility),
	})
	if err != nil {
		return nil, policyErrorStatus(err)
	}
	return toDecisionPB(d), nil
}

func (s *policyServer) GetDecision(ctx context.Context, req *policypb.GetDecisionRequest) (*policypb.Decision, error) {
	d, err := s.Policy.Get(ctx, req.Id)
	if err != nil {
		return nil, policyErrorStatus(err)
	}
	return toDecisionPB(d), nil
}

func (s *policyServer) Consume(ctx context.Context, req *policypb.ConsumeRequest) (*policypb.ConsumeResponse, error) {
	payload, err := unmarshalPayload(req.PayloadJson)
	if err != nil {
		return nil, err
	}
	digest, err := policy.Digest(payload)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.Policy.Consume(ctx, req.DecisionId, digest); err != nil {
		return nil, policyErrorStatus(err)
	}
	return &policypb.ConsumeResponse{}, nil
}

func (s *policyServer) ListPendingApprovals(ctx context.Context, req *policypb.ListPendingApprovalsRequest) (*policypb.ListPendingApprovalsResponse, error) {
	list, err := s.Policy.ListPendingApprovals(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*policypb.ApprovalRequest, 0, len(list))
	for i := range list {
		out = append(out, toApprovalRequestPB(&list[i]))
	}
	return &policypb.ListPendingApprovalsResponse{Approvals: out}, nil
}

func (s *policyServer) GetApprovalRequest(ctx context.Context, req *policypb.GetApprovalRequestRequest) (*policypb.ApprovalRequest, error) {
	a, err := s.Policy.GetApprovalRequest(ctx, req.Id)
	if err != nil {
		return nil, policyErrorStatus(err)
	}
	return toApprovalRequestPB(a), nil
}

func (s *policyServer) Approve(ctx context.Context, req *policypb.ResolveApprovalRequest) (*policypb.Decision, error) {
	d, err := s.Policy.Approve(ctx, req.ApprovalId, req.ResolvedBy)
	if err != nil {
		return nil, policyErrorStatus(err)
	}
	return toDecisionPB(d), nil
}

func (s *policyServer) Deny(ctx context.Context, req *policypb.ResolveApprovalRequest) (*policypb.ApprovalRequest, error) {
	a, err := s.Policy.Deny(ctx, req.ApprovalId, req.ResolvedBy, req.Reason)
	if err != nil {
		return nil, policyErrorStatus(err)
	}
	return toApprovalRequestPB(a), nil
}
