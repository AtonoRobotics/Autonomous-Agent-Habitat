package grpcapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/credentials"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/grpcapi/inferencepb"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/inference"
)

// testCredentials/registerProviderAccount mirror daemon/inference's own
// test helpers (inference_test.go) exactly — same real Postgres-backed
// credentials.Store, same envelope shape a real operator would register.
func testCredentials(t *testing.T) *credentials.Store {
	t.Helper()
	db := testDB(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	s, err := credentials.New(db, key)
	if err != nil {
		t.Fatalf("credentials.New: %v", err)
	}
	return s
}

func registerProviderAccount(t *testing.T, creds *credentials.Store, provider string, envelope any) {
	t.Helper()
	ctx := context.Background()
	acct, err := creds.CreateAccount(ctx, provider, "test")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	blob, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := creds.PutCredential(ctx, credentials.SubjectAccount, acct.ID, blob); err != nil {
		t.Fatalf("PutCredential: %v", err)
	}
}

// newTestInferenceClient mirrors newTestClient/newTestOperationsClient/
// newTestSelfImproveClient: a real grpc.Server backed by a real
// inference.Router (itself backed by a real Postgres-backed
// credentials.Store), reached over an in-memory bufconn listener.
func newTestInferenceClient(t *testing.T, router *inference.Router) inferencepb.InferenceServiceClient {
	t.Helper()

	auth, err := authn.New(testAgentToken, testOperatorToken)
	if err != nil {
		t.Fatalf("authn.New: %v", err)
	}

	grpcSrv := grpc.NewServer(grpc.UnaryInterceptor(authInterceptor(auth, inferenceRoles())))
	inferencepb.RegisterInferenceServiceServer(grpcSrv, &inferenceServer{Inference: router})

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

	return inferencepb.NewInferenceServiceClient(conn)
}

func TestComplete_Anthropic_RealRequestShapeAndResponseOverGRPC(t *testing.T) {
	var capturedAuth string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"the real answer"}],"usage":{"input_tokens":12,"output_tokens":3}}`))
	}))
	defer fake.Close()

	creds := testCredentials(t)
	registerProviderAccount(t, creds, "anthropic", map[string]string{"kind": "anthropic", "api_key": "sk-ant-test", "base_url": fake.URL})
	c := newTestInferenceClient(t, inference.New(creds))
	ctx := withToken(context.Background(), testAgentToken)

	resp, err := c.Complete(ctx, &inferencepb.CompleteRequest{
		Provider: "anthropic", Model: "claude-sonnet-5", System: "be helpful",
		Messages: []*inferencepb.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "the real answer" {
		t.Fatalf("expected real answer text, got %q", resp.Text)
	}
	if resp.InputTokens != 12 || resp.OutputTokens != 3 {
		t.Fatalf("expected real usage to round-trip, got in=%d out=%d", resp.InputTokens, resp.OutputTokens)
	}
	// §2.1/§14: claude-sonnet-5 pricing.go rate is $3/M input, $15/M
	// output — 12 in + 3 out is 12*3e-6 + 3*15e-6 = $0.000081.
	if want := 0.000081; resp.CostUsd < want-1e-9 || resp.CostUsd > want+1e-9 {
		t.Fatalf("expected cost_usd ~%v, got %v", want, resp.CostUsd)
	}
	if capturedAuth != "sk-ant-test" {
		t.Fatalf("expected the real api key to reach the provider, got %q", capturedAuth)
	}
}

func TestComplete_MissingModel_ReturnsInvalidArgument(t *testing.T) {
	c := newTestInferenceClient(t, inference.New(testCredentials(t)))
	ctx := withToken(context.Background(), testAgentToken)

	_, err := c.Complete(ctx, &inferencepb.CompleteRequest{Provider: "anthropic"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestComplete_NoAccountRegistered_ReturnsNotFound(t *testing.T) {
	c := newTestInferenceClient(t, inference.New(testCredentials(t)))
	ctx := withToken(context.Background(), testAgentToken)

	_, err := c.Complete(ctx, &inferencepb.CompleteRequest{Provider: "anthropic", Model: "claude-sonnet-5", Messages: []*inferencepb.Message{{Role: "user", Content: "hi"}}})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound for an unconfigured provider, got %v", err)
	}
}

func TestComplete_NoToken_ReturnsUnauthenticated(t *testing.T) {
	c := newTestInferenceClient(t, inference.New(testCredentials(t)))

	_, err := c.Complete(context.Background(), &inferencepb.CompleteRequest{Provider: "anthropic", Model: "claude-sonnet-5"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestComplete_RouterNil_ReturnsUnavailable(t *testing.T) {
	c := newTestInferenceClient(t, nil)
	ctx := withToken(context.Background(), testAgentToken)

	_, err := c.Complete(ctx, &inferencepb.CompleteRequest{Provider: "anthropic", Model: "claude-sonnet-5"})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable when inference is not configured, got %v", err)
	}
}

func TestCountTokens_Anthropic_RealRequestOverGRPC(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer fake.Close()

	creds := testCredentials(t)
	registerProviderAccount(t, creds, "anthropic", map[string]string{"kind": "anthropic", "api_key": "sk-ant-test", "base_url": fake.URL})
	c := newTestInferenceClient(t, inference.New(creds))
	ctx := withToken(context.Background(), testAgentToken)

	resp, err := c.CountTokens(ctx, &inferencepb.CompleteRequest{
		Provider: "anthropic", Model: "claude-sonnet-5",
		Messages: []*inferencepb.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if resp.InputTokens != 42 {
		t.Fatalf("expected 42, got %d", resp.InputTokens)
	}
}

func TestEmbed_OpenAICompatible_RealRequestOverGRPC(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3],"index":0}]}`))
	}))
	defer fake.Close()

	creds := testCredentials(t)
	registerProviderAccount(t, creds, "embedder", map[string]string{"kind": "openai_compatible", "api_key": "k", "base_url": fake.URL})
	c := newTestInferenceClient(t, inference.New(creds))
	ctx := withToken(context.Background(), testAgentToken)

	resp, err := c.Embed(ctx, &inferencepb.EmbedRequest{Provider: "embedder", Model: "text-embed", Input: []string{"hello"}})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if resp.Dimension != 3 || len(resp.Embeddings) != 1 || len(resp.Embeddings[0].Values) != 3 {
		t.Fatalf("expected one real 3-dimensional embedding, got %+v", resp)
	}
	if resp.Embeddings[0].Values[1] != float32(0.2) {
		t.Fatalf("expected the real embedding values to round-trip, got %v", resp.Embeddings[0].Values)
	}
}

func TestEmbed_MissingInput_ReturnsInvalidArgument(t *testing.T) {
	c := newTestInferenceClient(t, inference.New(testCredentials(t)))
	ctx := withToken(context.Background(), testAgentToken)

	_, err := c.Embed(ctx, &inferencepb.EmbedRequest{Provider: "embedder", Model: "text-embed"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestEmbed_AnthropicNotSupported_ReturnsInvalidArgument(t *testing.T) {
	creds := testCredentials(t)
	registerProviderAccount(t, creds, "anthropic", map[string]string{"kind": "anthropic", "api_key": "sk-ant-test", "base_url": "http://example.invalid"})
	c := newTestInferenceClient(t, inference.New(creds))
	ctx := withToken(context.Background(), testAgentToken)

	_, err := c.Embed(ctx, &inferencepb.EmbedRequest{Provider: "anthropic", Model: "text-embed", Input: []string{"hi"}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for embeddings on an anthropic-kind account, got %v", err)
	}
}
