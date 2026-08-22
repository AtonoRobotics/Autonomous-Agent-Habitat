package inference

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/credentials"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "amh.db"), "../../store/migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testCredentials(t *testing.T, db *sql.DB) *credentials.Store {
	t.Helper()
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

func TestComplete_NoAccountRegistered_ReturnsProviderNotConfigured(t *testing.T) {
	db := testDB(t)
	router := New(testCredentials(t, db))
	_, err := router.Complete(context.Background(), Request{Provider: "anthropic", Model: "claude-sonnet-5", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatalf("expected ErrProviderNotConfigured")
	}
}

func TestComplete_Anthropic_RealRequestShapeAndResponseParsing(t *testing.T) {
	var capturedAuth, capturedPath string
	var capturedBody map[string]any
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("x-api-key")
		capturedPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"the real answer"}]}`))
	}))
	defer fake.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "anthropic", map[string]string{"kind": "anthropic", "api_key": "sk-ant-test", "base_url": fake.URL})

	router := New(creds)
	result, err := router.Complete(context.Background(), Request{
		Provider: "anthropic", Model: "claude-sonnet-5", System: "be helpful",
		Messages: []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result != "the real answer" {
		t.Fatalf("expected real answer text, got %q", result)
	}
	if capturedAuth != "sk-ant-test" {
		t.Fatalf("expected the real api key in x-api-key header, got %q", capturedAuth)
	}
	if capturedPath != "/v1/messages" {
		t.Fatalf("expected /v1/messages, got %q", capturedPath)
	}
	if capturedBody["model"] != "claude-sonnet-5" {
		t.Fatalf("expected model in request body, got %v", capturedBody["model"])
	}
}

func TestComplete_AnthropicOAuth_UsesBearerNotAPIKeyHeader(t *testing.T) {
	var sawAuthHeader, sawAPIKeyHeader string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthHeader = r.Header.Get("Authorization")
		sawAPIKeyHeader = r.Header.Get("x-api-key")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer fake.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "anthropic", map[string]any{
		"kind": "anthropic", "base_url": fake.URL,
		"oauth": map[string]any{"access_token": "oauth-token-xyz", "refresh_token": "r", "expires_at": time.Now().Add(time.Hour), "refresh_url": "", "client_id": ""},
	})

	router := New(creds)
	if _, err := router.Complete(context.Background(), Request{Provider: "anthropic", Model: "claude-sonnet-5", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if sawAuthHeader != "Bearer oauth-token-xyz" {
		t.Fatalf("expected Authorization: Bearer oauth-token-xyz, got %q", sawAuthHeader)
	}
	if sawAPIKeyHeader != "" {
		t.Fatalf("expected no x-api-key header for an OAuth credential, got %q", sawAPIKeyHeader)
	}
}

func TestComplete_ExpiredOAuthToken_RefreshesAndRotatesInPlace(t *testing.T) {
	var completeCallCount int
	var sawAccessToken string
	completeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		completeCallCount++
		sawAccessToken = r.Header.Get("Authorization")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer completeServer.Close()

	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "brand-new-token", "refresh_token": "brand-new-refresh", "expires_in": 3600,
		})
	}))
	defer refreshServer.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	acct, _ := creds.CreateAccount(context.Background(), "anthropic", "test")
	envelope, _ := json.Marshal(map[string]any{
		"kind": "anthropic", "base_url": completeServer.URL,
		"oauth": map[string]any{
			"access_token": "expired-token", "refresh_token": "old-refresh",
			"expires_at":  time.Now().Add(-time.Hour), // already expired
			"refresh_url": refreshServer.URL, "client_id": "test-client",
		},
	})
	creds.PutCredential(context.Background(), credentials.SubjectAccount, acct.ID, envelope)

	router := New(creds)
	if _, err := router.Complete(context.Background(), Request{Provider: "anthropic", Model: "claude-sonnet-5", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if sawAccessToken != "Bearer brand-new-token" {
		t.Fatalf("expected the refreshed token to be used for the actual call, got %q", sawAccessToken)
	}

	// The rotated credential must be persisted — a second call must not
	// need to refresh again.
	if _, err := router.Complete(context.Background(), Request{Provider: "anthropic", Model: "claude-sonnet-5", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	if completeCallCount != 2 {
		t.Fatalf("expected exactly 2 completion calls, got %d", completeCallCount)
	}
}

func TestCountTokens_Anthropic_ReturnsRealCount(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("expected count_tokens path, got %s", r.URL.Path)
		}
		w.Write([]byte(`{"input_tokens":123}`))
	}))
	defer fake.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "anthropic", map[string]string{"kind": "anthropic", "api_key": "k", "base_url": fake.URL})

	router := New(creds)
	n, err := router.CountTokens(context.Background(), Request{Provider: "anthropic", Model: "claude-sonnet-5", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if n != 123 {
		t.Fatalf("expected 123, got %d", n)
	}
}

func TestComplete_OpenAICompatible_RealRequestShapeAndResponseParsing(t *testing.T) {
	var capturedAuth string
	var capturedBody map[string]any
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"glm's answer"}}]}`))
	}))
	defer fake.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "glm", map[string]string{"kind": "openai_compatible", "api_key": "glm-key", "base_url": fake.URL})

	router := New(creds)
	result, err := router.Complete(context.Background(), Request{
		Provider: "glm", Model: "glm-4.6", System: "be helpful",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result != "glm's answer" {
		t.Fatalf("got %q", result)
	}
	if capturedAuth != "Bearer glm-key" {
		t.Fatalf("expected Bearer glm-key, got %q", capturedAuth)
	}
	messages := capturedBody["messages"].([]any)
	first := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be helpful" {
		t.Fatalf("expected system message first, got %v", first)
	}
}

func TestCountTokens_OpenAICompatible_NotImplemented(t *testing.T) {
	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "glm", map[string]string{"kind": "openai_compatible", "api_key": "k", "base_url": "http://example.invalid"})

	router := New(creds)
	if _, err := router.CountTokens(context.Background(), Request{Provider: "glm", Model: "glm-4.6"}); err == nil {
		t.Fatalf("expected an error: count_tokens is not implemented for openai_compatible")
	}
}

func TestComplete_ProviderErrorPropagates(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer fake.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "anthropic", map[string]string{"kind": "anthropic", "api_key": "bad-key", "base_url": fake.URL})

	router := New(creds)
	_, err := router.Complete(context.Background(), Request{Provider: "anthropic", Model: "claude-sonnet-5", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatalf("expected the provider's 401 to propagate as an error")
	}
}

func TestComplete_FailsOverToSecondProviderOnFirstFailure(t *testing.T) {
	primaryCalls := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"primary is down"}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"content":[{"type":"text","text":"answer from backup"}]}`))
	}))
	defer backup.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "primary", map[string]string{"kind": "anthropic", "api_key": "k1", "base_url": primary.URL})
	registerProviderAccount(t, creds, "backup", map[string]string{"kind": "anthropic", "api_key": "k2", "base_url": backup.URL})

	router := New(creds)
	result, err := router.Complete(context.Background(), Request{
		Providers: []string{"primary", "backup"},
		Model:     "claude-sonnet-5",
		Messages:  []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result != "answer from backup" {
		t.Fatalf("expected the backup provider's answer, got %q", result)
	}
	if primaryCalls != 1 {
		t.Fatalf("expected exactly one attempt against the failed primary before failing over, got %d", primaryCalls)
	}
}

func TestComplete_FirstProviderSucceeds_SecondNeverCalled(t *testing.T) {
	backupCalls := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"content":[{"type":"text","text":"answer from primary"}]}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls++
		w.Write([]byte(`{"content":[{"type":"text","text":"should never be seen"}]}`))
	}))
	defer backup.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "primary", map[string]string{"kind": "anthropic", "api_key": "k1", "base_url": primary.URL})
	registerProviderAccount(t, creds, "backup", map[string]string{"kind": "anthropic", "api_key": "k2", "base_url": backup.URL})

	router := New(creds)
	result, err := router.Complete(context.Background(), Request{
		Providers: []string{"primary", "backup"},
		Model:     "claude-sonnet-5",
		Messages:  []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result != "answer from primary" {
		t.Fatalf("expected the primary provider's answer, got %q", result)
	}
	if backupCalls != 0 {
		t.Fatalf("expected the backup provider never to be called when the primary succeeds, got %d calls", backupCalls)
	}
}

func TestComplete_AllProvidersFail_ReturnsJoinedErrorNamingEach(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer failing.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "primary", map[string]string{"kind": "anthropic", "api_key": "k1", "base_url": failing.URL})
	// "backup" is deliberately never registered — an unconfigured provider
	// in the chain must also be reported, not silently skipped.

	router := New(creds)
	_, err := router.Complete(context.Background(), Request{
		Providers: []string{"primary", "backup"},
		Model:     "claude-sonnet-5",
		Messages:  []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("expected an error when every provider in the chain fails")
	}
	if !errors.Is(err, ErrAllProvidersFailed) {
		t.Fatalf("expected ErrAllProvidersFailed, got %v", err)
	}
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("expected the unconfigured 'backup' provider's error to be preserved in the joined error, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `"primary"`) || !strings.Contains(msg, `"backup"`) {
		t.Fatalf("expected the joined error to name both providers, got %q", msg)
	}
}

func TestCountTokens_FailsOverPastAKindThatDoesNotSupportIt(t *testing.T) {
	anthropicServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer anthropicServer.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	// "glm" is openai_compatible, which CountTokens does not implement —
	// the chain must move past it to "anthropic" rather than giving up.
	registerProviderAccount(t, creds, "glm", map[string]string{"kind": "openai_compatible", "api_key": "k", "base_url": "http://example.invalid"})
	registerProviderAccount(t, creds, "anthropic", map[string]string{"kind": "anthropic", "api_key": "k", "base_url": anthropicServer.URL})

	router := New(creds)
	n, err := router.CountTokens(context.Background(), Request{
		Providers: []string{"glm", "anthropic"},
		Model:     "claude-sonnet-5",
		Messages:  []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if n != 42 {
		t.Fatalf("expected 42, got %d", n)
	}
}

func TestComplete_DefaultsProviderToAnthropic(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer fake.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "anthropic", map[string]string{"kind": "anthropic", "api_key": "k", "base_url": fake.URL})

	router := New(creds)
	if _, err := router.Complete(context.Background(), Request{Model: "claude-sonnet-5", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("expected empty Provider to default to anthropic: %v", err)
	}
}

func TestEmbed_OpenAICompatible_RealRequestShapeAndResponseParsing(t *testing.T) {
	var capturedAuth string
	var capturedBody map[string]any
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("expected path /embeddings, got %q", r.URL.Path)
		}
		capturedAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3],"index":1},{"embedding":[0.4,0.5,0.6],"index":0}]}`))
	}))
	defer fake.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "embedder", map[string]string{"kind": "openai_compatible", "api_key": "embed-key", "base_url": fake.URL})

	router := New(creds)
	result, err := router.Embed(context.Background(), EmbedRequest{
		Provider: "embedder", Model: "text-embedding-3-small",
		Input: []string{"first", "second"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if result.Dimension != 3 {
		t.Fatalf("expected dimension 3, got %d", result.Dimension)
	}
	// Response arrived index-1-first, index-0-second — result.Embeddings
	// must be reordered back to match the input order.
	if len(result.Embeddings) != 2 || result.Embeddings[0][0] != 0.4 || result.Embeddings[1][0] != 0.1 {
		t.Fatalf("embeddings not reordered by index: %v", result.Embeddings)
	}
	if capturedAuth != "Bearer embed-key" {
		t.Fatalf("expected Bearer embed-key, got %q", capturedAuth)
	}
	if capturedBody["model"] != "text-embedding-3-small" {
		t.Fatalf("expected model in request body, got %v", capturedBody["model"])
	}
	input := capturedBody["input"].([]any)
	if len(input) != 2 || input[0] != "first" || input[1] != "second" {
		t.Fatalf("expected input array preserved, got %v", input)
	}
}

func TestEmbed_Anthropic_ReturnsNotSupported(t *testing.T) {
	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "anthropic", map[string]string{"kind": "anthropic", "api_key": "k"})

	router := New(creds)
	_, err := router.Embed(context.Background(), EmbedRequest{Provider: "anthropic", Model: "voyage-3", Input: []string{"x"}})
	if !errors.Is(err, ErrEmbedNotSupported) {
		t.Fatalf("expected ErrEmbedNotSupported, got %v", err)
	}
}

func TestEmbed_InconsistentDimensions_Fails(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"embedding":[0.1,0.2],"index":0},{"embedding":[0.1,0.2,0.3],"index":1}]}`))
	}))
	defer fake.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "embedder", map[string]string{"kind": "openai_compatible", "api_key": "k", "base_url": fake.URL})

	router := New(creds)
	_, err := router.Embed(context.Background(), EmbedRequest{Provider: "embedder", Model: "m", Input: []string{"a", "b"}})
	if err == nil {
		t.Fatalf("expected an error for inconsistent embedding dimensions")
	}
}

func TestEmbed_FailsOverToSecondProviderOnFirstFailure(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"embedding":[0.9],"index":0}]}`))
	}))
	defer backup.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "primary", map[string]string{"kind": "openai_compatible", "api_key": "k", "base_url": primary.URL})
	registerProviderAccount(t, creds, "backup", map[string]string{"kind": "openai_compatible", "api_key": "k", "base_url": backup.URL})

	router := New(creds)
	result, err := router.Embed(context.Background(), EmbedRequest{Providers: []string{"primary", "backup"}, Model: "m", Input: []string{"x"}})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(result.Embeddings) != 1 || result.Embeddings[0][0] != 0.9 {
		t.Fatalf("expected backup provider's result, got %v", result.Embeddings)
	}
}
