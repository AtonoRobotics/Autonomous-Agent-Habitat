package grpcapi

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/extensions"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/grpcapi/operationspb"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/operations"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/policy"
)

// newTestOperationsClient mirrors newTestClient (policy_service_test.go):
// a real grpc.Server — authInterceptor + OperationsService registered —
// backed by a real Postgres-backed operations.Engine/extensions.Registry,
// reached over an in-memory bufconn listener. Returns the extensions
// registry too, sharing the same db, so a test can Activate a real
// extension against it and get back a capability token the gRPC server's
// own authorizeEffectOwner will actually recognize.
func newTestOperationsClient(t *testing.T) (operationspb.OperationsServiceClient, *extensions.Registry) {
	t.Helper()

	db := testDB(t)
	auth, err := authn.New(testAgentToken, testOperatorToken)
	if err != nil {
		t.Fatalf("authn.New: %v", err)
	}
	reg := extensions.New(db)

	grpcSrv := grpc.NewServer(grpc.UnaryInterceptor(authInterceptor(auth, operationsRoles())))
	operationspb.RegisterOperationsServiceServer(grpcSrv, &operationsServer{
		Operations: operations.New(db, policy.New(db)),
		Extensions: reg,
	})

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

	return operationspb.NewOperationsServiceClient(conn), reg
}

// activateRealProcessExtension launches a real OS process extension
// against reg (the exact registry backing the gRPC server under test) and
// returns its id and a real, currently-valid capability token — the gRPC
// mirror of daemon/api/operations_test.go's own activateRealProcessExtension
// and daemon/extensions/registry_test.go's
// TestActivate_ProcessIsolation_RealProcessReceivesAWorkingCapabilityToken,
// both of which this borrows its approach from directly.
func activateRealProcessExtension(t *testing.T, reg *extensions.Registry, id string) (extensionID, token string) {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	scriptFile := filepath.Join(dir, "echo-token.sh")
	script := "#!/bin/sh\nprintenv AMH_EXTENSION_TOKEN > " + tokenFile + "\nexec sleep 300\n"
	if err := os.WriteFile(scriptFile, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	m := extensions.Manifest{
		APIVersion: "amh/v1",
		Kind:       "Extension",
		Metadata:   extensions.Metadata{ID: id, Name: "Test Extension", Version: "1.0.0", Publisher: "amh-tests"},
		Spec: extensions.Spec{
			Entrypoint: scriptFile,
			Isolation:  extensions.IsolationProcess,
			Provides:   []extensions.CapabilityRef{},
			Requires:   []extensions.Requirement{},
			Compatibility: extensions.Compatibility{
				AMHCore: ">=0.1.0",
			},
		},
	}
	if _, err := reg.Discover(ctx, m); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, err := reg.Activate(ctx, id, "1.0.0"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	t.Cleanup(func() {
		reg.Quiesce(context.Background(), id, "1.0.0")
		reg.Dispose(context.Background(), id, "1.0.0")
	})

	var raw []byte
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		raw, err = os.ReadFile(tokenFile)
		if err == nil && len(raw) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(raw) == 0 {
		t.Fatalf("expected the real launched process to have written its AMH_EXTENSION_TOKEN env var to %s: %v", tokenFile, err)
	}
	return id, strings.TrimSpace(string(raw))
}

func withExtensionToken(ctx context.Context, agentToken, extToken string) context.Context {
	ctx = withToken(ctx, agentToken)
	return metadata.AppendToOutgoingContext(ctx, "x-amh-extension-token", extToken)
}

func TestPropose_AdmitsOverGRPC(t *testing.T) {
	c, _ := newTestOperationsClient(t)
	ctx := withToken(context.Background(), testAgentToken)

	eff, err := c.Propose(ctx, &operationspb.ProposeRequest{
		OperationId: "op-1", OwnerExtensionId: "amh.core/test", EffectType: "amh.test/do-thing",
		PayloadJson: `{"x":1}`, Reversibility: "verified", RetryClass: "never",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if eff.State != "admitted" {
		t.Fatalf("expected admitted, got %s", eff.State)
	}
}

func TestPropose_NeedsApprovalOverGRPC(t *testing.T) {
	c, _ := newTestOperationsClient(t)
	ctx := withToken(context.Background(), testAgentToken)

	eff, err := c.Propose(ctx, &operationspb.ProposeRequest{
		OperationId: "op-1", OwnerExtensionId: "amh.core/test", EffectType: "amh.test/do-thing",
		PayloadJson: `{"x":1}`, Reversibility: "none", RetryClass: "never",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if eff.State != "needs_approval" {
		t.Fatalf("expected needs_approval, got %s", eff.State)
	}
}

func TestPropose_MissingRetryClass_ReturnsInvalidArgument(t *testing.T) {
	c, _ := newTestOperationsClient(t)
	ctx := withToken(context.Background(), testAgentToken)

	_, err := c.Propose(ctx, &operationspb.ProposeRequest{
		OperationId: "op-1", OwnerExtensionId: "amh.core/test", EffectType: "amh.test/do-thing",
		PayloadJson: `{"x":1}`, Reversibility: "verified",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for a missing retry_class, got %v", err)
	}
}

func TestFullHappyPath_ProposeDispatchObserveConfirmOverGRPC(t *testing.T) {
	c, _ := newTestOperationsClient(t)
	ctx := withToken(context.Background(), testAgentToken)
	payload := `{"x":1}`

	eff, err := c.Propose(ctx, &operationspb.ProposeRequest{
		OperationId: "op-1", OwnerExtensionId: "amh.core/test", EffectType: "amh.test/do-thing",
		PayloadJson: payload, Reversibility: "verified", RetryClass: "never",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	eff, err = c.MarkDispatchPending(ctx, &operationspb.MarkDispatchPendingRequest{EffectId: eff.EffectId, PayloadJson: payload})
	if err != nil {
		t.Fatalf("MarkDispatchPending: %v", err)
	}
	if eff.State != "dispatch_pending" {
		t.Fatalf("expected dispatch_pending, got %s", eff.State)
	}

	eff, err = c.MarkDispatched(ctx, &operationspb.MarkDispatchedRequest{EffectId: eff.EffectId, ExternalCommandId: "cmd-123"})
	if err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}
	if eff.State != "dispatched" {
		t.Fatalf("expected dispatched, got %s", eff.State)
	}

	eff, err = c.MarkObserved(ctx, &operationspb.MarkObservedRequest{EffectId: eff.EffectId, ObservationRef: "ref-1", ObservationPayload: "the real response"})
	if err != nil {
		t.Fatalf("MarkObserved: %v", err)
	}
	if eff.State != "observed" {
		t.Fatalf("expected observed, got %s", eff.State)
	}

	eff, err = c.Resolve(ctx, &operationspb.ResolveRequest{EffectId: eff.EffectId, Terminal: "confirmed"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if eff.State != "confirmed" {
		t.Fatalf("expected confirmed, got %s", eff.State)
	}

	// Durable reconstruction: fetched fresh, not read from the in-memory result.
	fetched, err := c.GetEffect(ctx, &operationspb.GetEffectRequest{EffectId: eff.EffectId})
	if err != nil {
		t.Fatalf("GetEffect: %v", err)
	}
	if fetched.ObservationPayload != "the real response" {
		t.Fatalf("expected the observation payload to be durably reconstructible, got %q", fetched.ObservationPayload)
	}
}

func TestMarkDispatchPending_PayloadMutatedAfterAdmission_FailsClosed(t *testing.T) {
	c, _ := newTestOperationsClient(t)
	ctx := withToken(context.Background(), testAgentToken)

	eff, err := c.Propose(ctx, &operationspb.ProposeRequest{
		OperationId: "op-1", OwnerExtensionId: "amh.core/test", EffectType: "amh.test/do-thing",
		PayloadJson: `{"x":1}`, Reversibility: "verified", RetryClass: "never",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	_, err = c.MarkDispatchPending(ctx, &operationspb.MarkDispatchPendingRequest{EffectId: eff.EffectId, PayloadJson: `{"x":99}`})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition on a mutated dispatch payload, got %v", err)
	}
}

func TestGetEffect_UnknownID_ReturnsNotFound(t *testing.T) {
	c, _ := newTestOperationsClient(t)
	ctx := withToken(context.Background(), testAgentToken)

	_, err := c.GetEffect(ctx, &operationspb.GetEffectRequest{EffectId: "does-not-exist"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestListEffectsByOperation_OverGRPC(t *testing.T) {
	c, _ := newTestOperationsClient(t)
	ctx := withToken(context.Background(), testAgentToken)

	if _, err := c.Propose(ctx, &operationspb.ProposeRequest{
		OperationId: "op-list", OwnerExtensionId: "amh.core/test", EffectType: "amh.test/do-thing",
		PayloadJson: `{"x":1}`, Reversibility: "verified", RetryClass: "never",
	}); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	resp, err := c.ListEffectsByOperation(ctx, &operationspb.ListEffectsByOperationRequest{OperationId: "op-list"})
	if err != nil {
		t.Fatalf("ListEffectsByOperation: %v", err)
	}
	if len(resp.Effects) != 1 {
		t.Fatalf("expected exactly one effect, got %d", len(resp.Effects))
	}
}

func TestListEffectsByOperation_RequiresOperationID(t *testing.T) {
	c, _ := newTestOperationsClient(t)
	ctx := withToken(context.Background(), testAgentToken)

	_, err := c.ListEffectsByOperation(ctx, &operationspb.ListEffectsByOperationRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestPropose_NonCoreOwner_AgentTokenWithNoExtensionToken_IsPermissionDenied(t *testing.T) {
	c, _ := newTestOperationsClient(t)
	ctx := withToken(context.Background(), testAgentToken)

	_, err := c.Propose(ctx, &operationspb.ProposeRequest{
		OperationId: "op-authz", OwnerExtensionId: "amh.acme/widget", EffectType: "amh.test/do-thing",
		PayloadJson: `{"x":1}`, Reversibility: "verified", RetryClass: "never",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied proposing on behalf of a non-core owner with no extension token, got %v", err)
	}
}

func TestPropose_NonCoreOwner_WrongExtensionsToken_IsPermissionDenied(t *testing.T) {
	c, reg := newTestOperationsClient(t)
	_, otherToken := activateRealProcessExtension(t, reg, "amh.acme/other-widget")
	ctx := withExtensionToken(context.Background(), testAgentToken, otherToken)

	_, err := c.Propose(ctx, &operationspb.ProposeRequest{
		OperationId: "op-authz", OwnerExtensionId: "amh.acme/widget", EffectType: "amh.test/do-thing",
		PayloadJson: `{"x":1}`, Reversibility: "verified", RetryClass: "never",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied presenting a different extension's own token, got %v", err)
	}
}

func TestPropose_NonCoreOwner_CorrectExtensionsToken_Succeeds(t *testing.T) {
	c, reg := newTestOperationsClient(t)
	extID, token := activateRealProcessExtension(t, reg, "amh.acme/widget")
	ctx := withExtensionToken(context.Background(), testAgentToken, token)

	eff, err := c.Propose(ctx, &operationspb.ProposeRequest{
		OperationId: "op-authz", OwnerExtensionId: extID, EffectType: "amh.test/do-thing",
		PayloadJson: `{"x":1}`, Reversibility: "verified", RetryClass: "never",
	})
	if err != nil {
		t.Fatalf("expected success presenting the matching extension's own token, got %v", err)
	}
	if eff.OwnerExtensionId != extID {
		t.Fatalf("expected owner %s, got %s", extID, eff.OwnerExtensionId)
	}
}

func TestPropose_NonCoreOwner_OperatorTokenAlwaysAllowed(t *testing.T) {
	c, _ := newTestOperationsClient(t)
	ctx := withToken(context.Background(), testOperatorToken)

	_, err := c.Propose(ctx, &operationspb.ProposeRequest{
		OperationId: "op-authz", OwnerExtensionId: "amh.acme/widget", EffectType: "amh.test/do-thing",
		PayloadJson: `{"x":1}`, Reversibility: "verified", RetryClass: "never",
	})
	if err != nil {
		t.Fatalf("expected an operator token to always be authorized, got %v", err)
	}
}

func TestMarkDispatchPending_NonCoreOwner_RequiresTheOwningExtensionsToken(t *testing.T) {
	c, reg := newTestOperationsClient(t)
	extID, token := activateRealProcessExtension(t, reg, "amh.acme/widget")
	ownerCtx := withExtensionToken(context.Background(), testAgentToken, token)

	eff, err := c.Propose(ownerCtx, &operationspb.ProposeRequest{
		OperationId: "op-authz", OwnerExtensionId: extID, EffectType: "amh.test/do-thing",
		PayloadJson: `{"x":1}`, Reversibility: "verified", RetryClass: "never",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	agentOnlyCtx := withToken(context.Background(), testAgentToken)
	_, err = c.MarkDispatchPending(agentOnlyCtx, &operationspb.MarkDispatchPendingRequest{EffectId: eff.EffectId, PayloadJson: `{"x":1}`})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied marking dispatch-pending with no extension token, got %v", err)
	}

	_, err = c.MarkDispatchPending(ownerCtx, &operationspb.MarkDispatchPendingRequest{EffectId: eff.EffectId, PayloadJson: `{"x":1}`})
	if err != nil {
		t.Fatalf("expected success marking dispatch-pending with the owning extension's own token, got %v", err)
	}
}
