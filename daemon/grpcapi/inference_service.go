package grpcapi

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/grpcapi/inferencepb"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/inference"
)

// inferenceServer implements inferencepb.InferenceServiceServer over
// daemon/inference.Router — the same model-provider seam
// daemon/api/controlplane.go's handleInference* handlers already expose
// over HTTP, including that package's own §4 external-effect tracking
// (Router.Complete/CountTokens/Embed wrap every provider attempt via
// trackEffect internally — nothing here needs to duplicate that).
type inferenceServer struct {
	inferencepb.UnimplementedInferenceServiceServer
	Inference *inference.Router
}

// inferenceRoles: all three RPCs are agent-or-operator, matching
// daemon/api/controlplane.go's /v1/inference/* routes exactly — an
// ephemeral agent computer calls these with only its agent bearer token,
// never holding a model-provider credential itself.
func inferenceRoles() requiredRoles {
	full := "/amh.inference.v1.InferenceService/"
	agentOrOperator := []authn.Role{authn.RoleAgent, authn.RoleOperator}
	return requiredRoles{
		full + "Complete":    agentOrOperator,
		full + "CountTokens": agentOrOperator,
		full + "Embed":       agentOrOperator,
	}
}

// inferenceUnavailable mirrors daemon/api/controlplane.go's identically-
// named method: Router is nil (AMH_CREDENTIAL_KEY unset) is a soft
// disable of this whole surface, not a startup refusal — reported as
// Unavailable, the gRPC status gRPC's own documentation recommends for
// "the service is currently unavailable," rather than treating a
// deliberately-unconfigured seam as a caller error.
func (s *inferenceServer) inferenceUnavailable() error {
	if s.Inference != nil {
		return nil
	}
	return status.Error(codes.Unavailable, "inference is not configured (AMH_CREDENTIAL_KEY unset) — the gRPC inference surface is disabled")
}

func inferenceErrorStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case status.Code(err) != codes.Unknown:
		return err
	case errors.Is(err, inference.ErrEmbedNotSupported):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, inference.ErrProviderNotConfigured):
		return status.Error(codes.NotFound, err.Error())
	default:
		// Mirrors handleInference*'s http.StatusBadGateway default: the
		// daemon reached out to an upstream provider and that attempt
		// itself failed — Unavailable is gRPC's documented code for "the
		// service (here, the upstream provider) is currently unavailable."
		return status.Error(codes.Unavailable, err.Error())
	}
}

func toInferenceMessages(in []*inferencepb.Message) []inference.Message {
	out := make([]inference.Message, len(in))
	for i, m := range in {
		out[i] = inference.Message{Role: m.Role, Content: m.Content}
	}
	return out
}

func (s *inferenceServer) Complete(ctx context.Context, req *inferencepb.CompleteRequest) (*inferencepb.CompleteResponse, error) {
	if err := s.inferenceUnavailable(); err != nil {
		return nil, err
	}
	if req.Model == "" {
		return nil, status.Error(codes.InvalidArgument, "model is required")
	}
	text, usage, err := s.Inference.Complete(ctx, inference.Request{
		Provider: req.Provider, Providers: req.Providers, Model: req.Model,
		System: req.System, Messages: toInferenceMessages(req.Messages), MaxTokens: int(req.MaxTokens),
	})
	if err != nil {
		return nil, inferenceErrorStatus(err)
	}
	return &inferencepb.CompleteResponse{Text: text, InputTokens: int32(usage.InputTokens), OutputTokens: int32(usage.OutputTokens)}, nil
}

func (s *inferenceServer) CountTokens(ctx context.Context, req *inferencepb.CompleteRequest) (*inferencepb.CountTokensResponse, error) {
	if err := s.inferenceUnavailable(); err != nil {
		return nil, err
	}
	if req.Model == "" {
		return nil, status.Error(codes.InvalidArgument, "model is required")
	}
	n, err := s.Inference.CountTokens(ctx, inference.Request{
		Provider: req.Provider, Providers: req.Providers, Model: req.Model,
		System: req.System, Messages: toInferenceMessages(req.Messages),
	})
	if err != nil {
		return nil, inferenceErrorStatus(err)
	}
	return &inferencepb.CountTokensResponse{InputTokens: int32(n)}, nil
}

func (s *inferenceServer) Embed(ctx context.Context, req *inferencepb.EmbedRequest) (*inferencepb.EmbedResponse, error) {
	if err := s.inferenceUnavailable(); err != nil {
		return nil, err
	}
	if req.Model == "" {
		return nil, status.Error(codes.InvalidArgument, "model is required")
	}
	if len(req.Input) == 0 {
		return nil, status.Error(codes.InvalidArgument, "input is required")
	}
	result, err := s.Inference.Embed(ctx, inference.EmbedRequest{
		Provider: req.Provider, Providers: req.Providers, Model: req.Model, Input: req.Input,
	})
	if err != nil {
		return nil, inferenceErrorStatus(err)
	}
	vectors := make([]*inferencepb.EmbedFloatVector, len(result.Embeddings))
	for i, v := range result.Embeddings {
		vectors[i] = &inferencepb.EmbedFloatVector{Values: v}
	}
	return &inferencepb.EmbedResponse{Embeddings: vectors, Dimension: int32(result.Dimension)}, nil
}
