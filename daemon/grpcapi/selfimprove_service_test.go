package grpcapi

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/grpcapi/selfimprovepb"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/selfimprove"
)

// newTestSelfImproveClient mirrors newTestClient (policy_service_test.go)
// and newTestOperationsClient (operations_service_test.go): a real
// grpc.Server backed by a real Postgres-backed selfimprove.Engine, reached
// over an in-memory bufconn listener.
func newTestSelfImproveClient(t *testing.T) (selfimprovepb.SelfImproveServiceClient, *selfimprove.Engine) {
	t.Helper()

	db := testDB(t)
	auth, err := authn.New(testAgentToken, testOperatorToken)
	if err != nil {
		t.Fatalf("authn.New: %v", err)
	}
	eng := selfimprove.New(db)

	grpcSrv := grpc.NewServer(grpc.UnaryInterceptor(authInterceptor(auth, selfimproveRoles())))
	selfimprovepb.RegisterSelfImproveServiceServer(grpcSrv, &selfimproveServer{SelfImprove: eng})

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

	return selfimprovepb.NewSelfImproveServiceClient(conn), eng
}

func TestGetCandidate_RoundTripsOverGRPC(t *testing.T) {
	c, eng := newTestSelfImproveClient(t)
	ctx := withToken(context.Background(), testAgentToken)

	created, err := eng.Generate(context.Background(), selfimprove.ClassPrompt, "some prompt text", "agent-1")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	got, err := c.GetCandidate(ctx, &selfimprovepb.GetCandidateRequest{Id: created.ID})
	if err != nil {
		t.Fatalf("GetCandidate: %v", err)
	}
	if got.Id != created.ID {
		t.Fatalf("expected id %s, got %s", created.ID, got.Id)
	}
	if got.Ref != "some prompt text" {
		t.Fatalf("expected ref to round-trip, got %q", got.Ref)
	}
	if got.Status != "generated" {
		t.Fatalf("expected a freshly generated candidate to be status generated, got %s", got.Status)
	}
}

func TestGetCandidate_UnknownID_ReturnsNotFound(t *testing.T) {
	c, _ := newTestSelfImproveClient(t)
	ctx := withToken(context.Background(), testAgentToken)

	_, err := c.GetCandidate(ctx, &selfimprovepb.GetCandidateRequest{Id: "does-not-exist"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestGetCandidate_NoToken_ReturnsUnauthenticated(t *testing.T) {
	c, _ := newTestSelfImproveClient(t)

	_, err := c.GetCandidate(context.Background(), &selfimprovepb.GetCandidateRequest{Id: "whatever"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestListCandidates_FiltersByClassAndStatusOverGRPC(t *testing.T) {
	c, eng := newTestSelfImproveClient(t)
	ctx := withToken(context.Background(), testAgentToken)
	bgctx := context.Background()

	if _, err := eng.Generate(bgctx, selfimprove.ClassPrompt, "prompt A", "agent-1"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := eng.Generate(bgctx, selfimprove.ClassSkill, "skill A", "agent-1"); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	promptOnly, err := c.ListCandidates(ctx, &selfimprovepb.ListCandidatesRequest{CandidateClass: "prompt"})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(promptOnly.Candidates) != 1 || promptOnly.Candidates[0].Ref != "prompt A" {
		t.Fatalf("expected exactly one prompt-class candidate, got %+v", promptOnly.Candidates)
	}

	generatedOnly, err := c.ListCandidates(ctx, &selfimprovepb.ListCandidatesRequest{Status: "generated"})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(generatedOnly.Candidates) != 2 {
		t.Fatalf("expected both freshly generated candidates, got %d", len(generatedOnly.Candidates))
	}

	promotedOnly, err := c.ListCandidates(ctx, &selfimprovepb.ListCandidatesRequest{Status: "promoted"})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(promotedOnly.Candidates) != 0 {
		t.Fatalf("expected no promoted candidates, got %d", len(promotedOnly.Candidates))
	}
}

func TestListCandidates_OperatorTokenAlsoAllowed(t *testing.T) {
	c, _ := newTestSelfImproveClient(t)
	ctx := withToken(context.Background(), testOperatorToken)

	if _, err := c.ListCandidates(ctx, &selfimprovepb.ListCandidatesRequest{}); err != nil {
		t.Fatalf("expected an operator token to be allowed on a read route, got %v", err)
	}
}
