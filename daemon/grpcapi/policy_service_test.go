package grpcapi

import (
	"context"
	"database/sql"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/grpcapi/policypb"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/policy"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store/storetest"
)

const (
	testAgentToken    = "test-agent-token"
	testOperatorToken = "test-operator-token"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return storetest.Open(t, "../../store/migrations")
}

// newTestClient builds a real grpc.Server (same wiring Server.Run does —
// authInterceptor + the policy service registered) backed by a real
// Postgres-backed policy.Engine, and a real grpc.ClientConn talking to it
// over an in-memory bufconn listener (no TCP port allocation needed for a
// test that only cares about this process's own client/server round trip).
func newTestClient(t *testing.T) policypb.PolicyServiceClient {
	t.Helper()

	auth, err := authn.New(testAgentToken, testOperatorToken)
	if err != nil {
		t.Fatalf("authn.New: %v", err)
	}

	grpcSrv := grpc.NewServer(grpc.UnaryInterceptor(authInterceptor(auth, policyRoles())))
	policypb.RegisterPolicyServiceServer(grpcSrv, &policyServer{Policy: policy.New(testDB(t))})

	lis := bufconn.Listen(1024 * 1024)
	go func() {
		_ = grpcSrv.Serve(lis)
	}()
	t.Cleanup(grpcSrv.Stop)

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return policypb.NewPolicyServiceClient(conn)
}

func withToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func TestDecide_VerifiedReversibility_AdmitsOverGRPC(t *testing.T) {
	c := newTestClient(t)
	ctx := withToken(context.Background(), testAgentToken)

	d, err := c.Decide(ctx, &policypb.DecideRequest{
		OperationId:   "op-1",
		PayloadJson:   `{"open_pct":60}`,
		Reversibility: "verified",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Result != "admit" {
		t.Fatalf("expected admit, got %s", d.Result)
	}
	if d.ApprovalRequestId != "" {
		t.Fatalf("expected no approval request for an admitted decision")
	}
}

func TestDecide_MissingOperationID_ReturnsInvalidArgument(t *testing.T) {
	c := newTestClient(t)
	ctx := withToken(context.Background(), testAgentToken)

	_, err := c.Decide(ctx, &policypb.DecideRequest{PayloadJson: `{}`, Reversibility: "verified"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestDecide_NoToken_ReturnsUnauthenticated(t *testing.T) {
	c := newTestClient(t)

	_, err := c.Decide(context.Background(), &policypb.DecideRequest{OperationId: "op-1", Reversibility: "verified"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestDecide_InvalidToken_ReturnsUnauthenticated(t *testing.T) {
	c := newTestClient(t)
	ctx := withToken(context.Background(), "not-a-real-token")

	_, err := c.Decide(ctx, &policypb.DecideRequest{OperationId: "op-1", Reversibility: "verified"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestConsume_RealPayloadRoundTrip_OverGRPC(t *testing.T) {
	c := newTestClient(t)
	ctx := withToken(context.Background(), testAgentToken)
	payload := `{"open_pct":60}`

	d, err := c.Decide(ctx, &policypb.DecideRequest{OperationId: "op-1", PayloadJson: payload, Reversibility: "verified"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if _, err := c.Consume(ctx, &policypb.ConsumeRequest{DecisionId: d.Id, PayloadJson: payload}); err != nil {
		t.Fatalf("Consume: %v", err)
	}

	// A second consume of the same decision fails closed.
	_, err = c.Consume(ctx, &policypb.ConsumeRequest{DecisionId: d.Id, PayloadJson: payload})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition on a repeat consume, got %v", err)
	}
}

func TestConsume_DifferentPayloadThanDecided_ReturnsFailedPrecondition(t *testing.T) {
	c := newTestClient(t)
	ctx := withToken(context.Background(), testAgentToken)

	d, err := c.Decide(ctx, &policypb.DecideRequest{OperationId: "op-1", PayloadJson: `{"open_pct":60}`, Reversibility: "verified"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	_, err = c.Consume(ctx, &policypb.ConsumeRequest{DecisionId: d.Id, PayloadJson: `{"open_pct":99}`})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition on a digest mismatch, got %v", err)
	}
}

func TestGetDecision_UnknownID_ReturnsNotFound(t *testing.T) {
	c := newTestClient(t)
	ctx := withToken(context.Background(), testAgentToken)

	_, err := c.GetDecision(ctx, &policypb.GetDecisionRequest{Id: "does-not-exist"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestNeedsApprovalLoop_AgentCannotSelfApprove_OperatorCan(t *testing.T) {
	c := newTestClient(t)
	agentCtx := withToken(context.Background(), testAgentToken)
	operatorCtx := withToken(context.Background(), testOperatorToken)
	payload := `{"ml":5}`

	d, err := c.Decide(agentCtx, &policypb.DecideRequest{OperationId: "op-1", PayloadJson: payload, Reversibility: "none"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Result != "needs_approval" {
		t.Fatalf("expected needs_approval, got %s", d.Result)
	}
	if d.ApprovalRequestId == "" {
		t.Fatalf("expected a bound approval request")
	}

	// Not yet consumable.
	if _, err := c.Consume(agentCtx, &policypb.ConsumeRequest{DecisionId: d.Id, PayloadJson: payload}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition consuming a not-yet-admitted decision, got %v", err)
	}

	// An agent token cannot approve its own request.
	if _, err := c.Approve(agentCtx, &policypb.ResolveApprovalRequest{ApprovalId: d.ApprovalRequestId}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for an agent approving its own request, got %v", err)
	}

	pending, err := c.ListPendingApprovals(agentCtx, &policypb.ListPendingApprovalsRequest{})
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	found := false
	for _, a := range pending.Approvals {
		if a.Id == d.ApprovalRequestId {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the pending approval to be listed")
	}

	approved, err := c.Approve(operatorCtx, &policypb.ResolveApprovalRequest{ApprovalId: d.ApprovalRequestId, ResolvedBy: "operator:jane"})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Result != "admit" {
		t.Fatalf("expected the freshly-minted decision to be admitted, got %s", approved.Result)
	}
	if approved.Id == d.Id {
		t.Fatalf("expected a fresh decision, not the original mutated in place")
	}

	// The original decision is untouched.
	original, err := c.GetDecision(agentCtx, &policypb.GetDecisionRequest{Id: d.Id})
	if err != nil {
		t.Fatalf("GetDecision: %v", err)
	}
	if original.Result != "needs_approval" {
		t.Fatalf("expected the original decision to remain needs_approval, got %s", original.Result)
	}

	// Now the freshly-minted decision is consumable.
	if _, err := c.Consume(agentCtx, &policypb.ConsumeRequest{DecisionId: approved.Id, PayloadJson: payload}); err != nil {
		t.Fatalf("Consume(approved): %v", err)
	}
}

func TestDeny_LeavesActionPermanentlyUnadmitted(t *testing.T) {
	c := newTestClient(t)
	agentCtx := withToken(context.Background(), testAgentToken)
	operatorCtx := withToken(context.Background(), testOperatorToken)
	payload := `{"x":1}`

	d, err := c.Decide(agentCtx, &policypb.DecideRequest{OperationId: "op-1", PayloadJson: payload, Reversibility: "claimed"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	denied, err := c.Deny(operatorCtx, &policypb.ResolveApprovalRequest{ApprovalId: d.ApprovalRequestId, ResolvedBy: "operator:jane", Reason: "too risky"})
	if err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if denied.Status != "denied" {
		t.Fatalf("expected denied, got %s", denied.Status)
	}
	if denied.Reason != "too risky" {
		t.Fatalf("expected the denial reason to round-trip, got %q", denied.Reason)
	}

	if _, err := c.Consume(agentCtx, &policypb.ConsumeRequest{DecisionId: d.Id, PayloadJson: payload}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition consuming a denied decision, got %v", err)
	}
}

func TestGetApprovalRequest_RoundTrips(t *testing.T) {
	c := newTestClient(t)
	agentCtx := withToken(context.Background(), testAgentToken)

	d, err := c.Decide(agentCtx, &policypb.DecideRequest{OperationId: "op-1", PayloadJson: `{"x":1}`, Reversibility: "none"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	ar, err := c.GetApprovalRequest(agentCtx, &policypb.GetApprovalRequestRequest{Id: d.ApprovalRequestId})
	if err != nil {
		t.Fatalf("GetApprovalRequest: %v", err)
	}
	if ar.Id != d.ApprovalRequestId {
		t.Fatalf("expected id %s, got %s", d.ApprovalRequestId, ar.Id)
	}
	if ar.Status != "pending" {
		t.Fatalf("expected pending, got %s", ar.Status)
	}
}

func TestDeny_AgentToken_ReturnsPermissionDenied(t *testing.T) {
	c := newTestClient(t)
	agentCtx := withToken(context.Background(), testAgentToken)

	d, err := c.Decide(agentCtx, &policypb.DecideRequest{OperationId: "op-1", PayloadJson: `{"x":1}`, Reversibility: "none"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	_, err = c.Deny(agentCtx, &policypb.ResolveApprovalRequest{ApprovalId: d.ApprovalRequestId})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}
