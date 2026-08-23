package grpcapi

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/grpcapi/selfimprovepb"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/selfimprove"
)

// selfimproveServer implements selfimprovepb.SelfImproveServiceServer over
// daemon/selfimprove.Engine — read-only (see selfimprove.proto's header
// comment for why only the two read RPCs moved here).
type selfimproveServer struct {
	selfimprovepb.UnimplementedSelfImproveServiceServer
	SelfImprove *selfimprove.Engine
}

// selfimproveRoles: both RPCs are agent-or-operator, matching
// daemon/api/selfimprove.go's GET routes exactly.
func selfimproveRoles() requiredRoles {
	full := "/amh.selfimprove.v1.SelfImproveService/"
	agentOrOperator := []authn.Role{authn.RoleAgent, authn.RoleOperator}
	return requiredRoles{
		full + "GetCandidate":   agentOrOperator,
		full + "ListCandidates": agentOrOperator,
	}
}

func selfimproveErrorStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case status.Code(err) != codes.Unknown:
		return err
	case errors.Is(err, selfimprove.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	default:
		return status.Error(codes.InvalidArgument, err.Error())
	}
}

func toCandidatePB(c *selfimprove.CandidateVersion) *selfimprovepb.CandidateVersion {
	return &selfimprovepb.CandidateVersion{
		Id: c.ID, CandidateClass: string(c.CandidateClass), Ref: c.Ref, Status: string(c.Status),
		GeneratedBy: c.GeneratedBy, CreatedAt: c.CreatedAt, CanaryAt: c.CanaryAt,
		PromotedAt: c.PromotedAt, DemotedAt: c.DemotedAt, RolledBackAt: c.RolledBackAt,
		RollbackTargetId: c.RollbackTargetID,
	}
}

func (s *selfimproveServer) GetCandidate(ctx context.Context, req *selfimprovepb.GetCandidateRequest) (*selfimprovepb.CandidateVersion, error) {
	c, err := s.SelfImprove.Get(ctx, req.Id)
	if err != nil {
		return nil, selfimproveErrorStatus(err)
	}
	return toCandidatePB(c), nil
}

func (s *selfimproveServer) ListCandidates(ctx context.Context, req *selfimprovepb.ListCandidatesRequest) (*selfimprovepb.ListCandidatesResponse, error) {
	list, err := s.SelfImprove.List(ctx, selfimprove.ListFilter{
		Class:  selfimprove.CandidateClass(req.CandidateClass),
		Status: selfimprove.Status(req.Status),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*selfimprovepb.CandidateVersion, 0, len(list))
	for i := range list {
		out = append(out, toCandidatePB(&list[i]))
	}
	return &selfimprovepb.ListCandidatesResponse{Candidates: out}, nil
}
