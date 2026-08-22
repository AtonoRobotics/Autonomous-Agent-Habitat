package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/credentials"
)

func newTestServer(t *testing.T, withCredentials bool) *httptest.Server {
	t.Helper()
	db := testDB(t)
	tp := sdktrace.NewTracerProvider()
	var creds *credentials.Store
	if withCredentials {
		key := make([]byte, 32)
		for i := range key {
			key[i] = byte(i)
		}
		var err error
		creds, err = credentials.New(db, key)
		if err != nil {
			t.Fatalf("credentials.New: %v", err)
		}
	}
	server := New("", db, tp, testAuth(t), nil, t.TempDir(), creds, false)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func baseTestManifest(id, version string) map[string]any {
	return map[string]any{
		"apiVersion": "amh/v1",
		"kind":       "Extension",
		"metadata": map[string]any{
			"id":        id,
			"name":      "Test Extension",
			"version":   version,
			"publisher": "amh-tests",
		},
		"spec": map[string]any{
			"entrypoint": "true",
			"isolation":  "in_process",
			"provides":   []any{},
			"requires":   []any{},
			"compatibility": map[string]any{
				"amhCore": ">=0.1.0",
			},
		},
	}
}

// ── Extensions ───────────────────────────────────────────────────────────

func TestExtensionLifecycle_DiscoverActivateQuiesceDispose_ViaHTTP(t *testing.T) {
	ts := newTestServer(t, false)

	body, _ := json.Marshal(baseTestManifest("amh.test/widget", "1.0.0"))
	discover := postJSON(t, ts.URL+"/v1/extensions", testOperatorToken, body)
	if discover.StatusCode != http.StatusCreated {
		t.Fatalf("discover: expected 201, got %d", discover.StatusCode)
	}
	discover.Body.Close()

	ref, _ := json.Marshal(map[string]string{"id": "amh.test/widget", "version": "1.0.0"})

	activate := postJSON(t, ts.URL+"/v1/extensions/activate", testOperatorToken, ref)
	if activate.StatusCode != http.StatusOK {
		t.Fatalf("activate: expected 200, got %d", activate.StatusCode)
	}
	var activated extensionResponse
	json.NewDecoder(activate.Body).Decode(&activated)
	activate.Body.Close()
	if activated.Status != "active" {
		t.Fatalf("expected active, got %s", activated.Status)
	}

	get := getJSON(t, ts.URL+"/v1/extensions/get?id=amh.test%2Fwidget&version=1.0.0", testAgentToken)
	if get.StatusCode != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", get.StatusCode)
	}
	get.Body.Close()

	quiesce := postJSON(t, ts.URL+"/v1/extensions/quiesce", testOperatorToken, ref)
	if quiesce.StatusCode != http.StatusOK {
		t.Fatalf("quiesce: expected 200, got %d", quiesce.StatusCode)
	}
	quiesce.Body.Close()

	dispose := postJSON(t, ts.URL+"/v1/extensions/dispose", testOperatorToken, ref)
	if dispose.StatusCode != http.StatusOK {
		t.Fatalf("dispose: expected 200, got %d", dispose.StatusCode)
	}
	var disposed extensionResponse
	json.NewDecoder(dispose.Body).Decode(&disposed)
	dispose.Body.Close()
	if disposed.Status != "disposed" {
		t.Fatalf("expected disposed, got %s", disposed.Status)
	}

	list := getJSON(t, ts.URL+"/v1/extensions", testAgentToken)
	var all []extensionResponse
	json.NewDecoder(list.Body).Decode(&all)
	list.Body.Close()
	if len(all) != 1 {
		t.Fatalf("expected 1 extension listed, got %d", len(all))
	}
}

func TestExtensionMutations_RejectAgentToken(t *testing.T) {
	ts := newTestServer(t, false)

	body, _ := json.Marshal(baseTestManifest("amh.test/widget", "1.0.0"))
	resp := postJSON(t, ts.URL+"/v1/extensions", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for an agent token installing an extension, got %d", resp.StatusCode)
	}
}

func TestExtensionActivate_MissingRequirementReturns409(t *testing.T) {
	ts := newTestServer(t, false)

	m := baseTestManifest("amh.test/consumer", "1.0.0")
	m["spec"].(map[string]any)["requires"] = []any{
		map[string]any{"capability": "amh.test/producer-cap", "versionRange": ">=1.0.0", "optional": false},
	}
	body, _ := json.Marshal(m)
	postJSON(t, ts.URL+"/v1/extensions", testOperatorToken, body).Body.Close()

	ref, _ := json.Marshal(map[string]string{"id": "amh.test/consumer", "version": "1.0.0"})
	resp := postJSON(t, ts.URL+"/v1/extensions/activate", testOperatorToken, ref)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for a missing dependency, got %d", resp.StatusCode)
	}
}

// ── Computers ────────────────────────────────────────────────────────────

func TestComputerLifecycle_CreateThenDestroy_ViaHTTP_AgentTokenAllowed(t *testing.T) {
	ts := newTestServer(t, false)

	createBody, _ := json.Marshal(map[string]any{
		"agent_id":  "",
		"isolation": "process",
		"image":     "sleep 60",
	})
	create := postJSON(t, ts.URL+"/v1/computers", testAgentToken, createBody)
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", create.StatusCode)
	}
	var c computerResponse
	json.NewDecoder(create.Body).Decode(&c)
	create.Body.Close()
	if c.Status != "ready" {
		t.Fatalf("expected ready, got %s", c.Status)
	}

	get := getJSON(t, ts.URL+"/v1/computers/"+c.ID, testAgentToken)
	if get.StatusCode != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", get.StatusCode)
	}
	get.Body.Close()

	destroyBody, _ := json.Marshal(map[string]string{"reason": "test done"})
	destroy := postJSON(t, ts.URL+"/v1/computers/"+c.ID+"/destroy", testAgentToken, destroyBody)
	if destroy.StatusCode != http.StatusOK {
		t.Fatalf("destroy: expected 200, got %d", destroy.StatusCode)
	}
	var destroyed computerResponse
	json.NewDecoder(destroy.Body).Decode(&destroyed)
	destroy.Body.Close()
	if destroyed.Status != "destroyed" {
		t.Fatalf("expected destroyed, got %s", destroyed.Status)
	}
}

// ── Accounts / credentials ───────────────────────────────────────────────

func TestAccountLifecycle_CreateAuthenticateRevoke_OperatorOnly(t *testing.T) {
	ts := newTestServer(t, true)

	createBody, _ := json.Marshal(map[string]string{"provider": "github", "display_name": "bot"})
	agentAttempt := postJSON(t, ts.URL+"/v1/accounts", testAgentToken, createBody)
	agentAttempt.Body.Close()
	if agentAttempt.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for an agent token creating an account, got %d", agentAttempt.StatusCode)
	}

	create := postJSON(t, ts.URL+"/v1/accounts", testOperatorToken, createBody)
	var acct accountResponse
	json.NewDecoder(create.Body).Decode(&acct)
	create.Body.Close()
	if acct.Status != "pending" {
		t.Fatalf("expected pending, got %s", acct.Status)
	}

	credBody, _ := json.Marshal(map[string]string{"secret": "ghp_supersecret"})
	putCred := postJSON(t, ts.URL+"/v1/accounts/"+acct.ID+"/credential", testOperatorToken, credBody)
	var activated accountResponse
	respBytes := mustReadAll(t, putCred.Body)
	putCred.Body.Close()
	json.Unmarshal(respBytes, &activated)
	if activated.Status != "active" {
		t.Fatalf("expected active after credential, got %s (body: %s)", activated.Status, respBytes)
	}
	if bytes.Contains(respBytes, []byte("supersecret")) {
		t.Fatalf("the secret must never be echoed back in the HTTP response: %s", respBytes)
	}

	list := getJSON(t, ts.URL+"/v1/accounts", testAgentToken)
	var all []accountResponse
	listBytes := mustReadAll(t, list.Body)
	list.Body.Close()
	json.Unmarshal(listBytes, &all)
	if bytes.Contains(listBytes, []byte("supersecret")) {
		t.Fatalf("listing accounts must never leak credential material: %s", listBytes)
	}

	revoke := postJSON(t, ts.URL+"/v1/accounts/"+acct.ID+"/revoke", testOperatorToken, nil)
	var revoked accountResponse
	json.NewDecoder(revoke.Body).Decode(&revoked)
	revoke.Body.Close()
	if revoked.Status != "revoked" {
		t.Fatalf("expected revoked, got %s", revoked.Status)
	}
}

func TestAccountRoutes_503WhenCredentialStoreDisabled(t *testing.T) {
	ts := newTestServer(t, false)
	resp := postJSON(t, ts.URL+"/v1/accounts", testOperatorToken, []byte(`{"provider":"github"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the credential store isn't configured, got %d", resp.StatusCode)
	}
}

// ── Inference ────────────────────────────────────────────────────────────

func registerFakeModelProvider(t *testing.T, ts *httptest.Server, providerFakeURL string) {
	t.Helper()
	registerFakeModelProviderNamed(t, ts, "test-fake", providerFakeURL)
}

func registerFakeModelProviderNamed(t *testing.T, ts *httptest.Server, provider, providerFakeURL string) {
	t.Helper()
	createBody, _ := json.Marshal(map[string]string{"provider": provider, "display_name": "fake provider"})
	create := postJSON(t, ts.URL+"/v1/accounts", testOperatorToken, createBody)
	var acct accountResponse
	json.NewDecoder(create.Body).Decode(&acct)
	create.Body.Close()

	envelope, _ := json.Marshal(map[string]string{"kind": "openai_compatible", "api_key": "test-key", "base_url": providerFakeURL})
	credBody, _ := json.Marshal(map[string]string{"secret": string(envelope)})
	putCred := postJSON(t, ts.URL+"/v1/accounts/"+acct.ID+"/credential", testOperatorToken, credBody)
	if putCred.StatusCode != http.StatusOK {
		t.Fatalf("register fake provider credential: expected 200, got %d", putCred.StatusCode)
	}
	putCred.Body.Close()
}

func TestInferenceComplete_RealRoundTripThroughRegisteredProvider(t *testing.T) {
	fakeProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"the real answer"}}]}`))
	}))
	defer fakeProvider.Close()

	ts := newTestServer(t, true)
	registerFakeModelProvider(t, ts, fakeProvider.URL)

	body, _ := json.Marshal(map[string]any{
		"provider": "test-fake", "model": "test-model", "system": "be helpful",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	resp := postJSON(t, ts.URL+"/v1/inference/complete", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result inferenceCompleteResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Text != "the real answer" {
		t.Fatalf("expected the real provider text, got %q", result.Text)
	}
}

func TestInferenceComplete_ProvidersFieldFailsOverOverHTTP(t *testing.T) {
	primaryCalls := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"answer from backup"}}]}`))
	}))
	defer backup.Close()

	ts := newTestServer(t, true)
	registerFakeModelProviderNamed(t, ts, "primary", primary.URL)
	registerFakeModelProviderNamed(t, ts, "backup", backup.URL)

	body, _ := json.Marshal(map[string]any{
		"providers": []string{"primary", "backup"}, "model": "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	resp := postJSON(t, ts.URL+"/v1/inference/complete", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after failing over to the backup provider, got %d", resp.StatusCode)
	}
	var result inferenceCompleteResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Text != "answer from backup" {
		t.Fatalf("expected the backup provider's answer, got %q", result.Text)
	}
	if primaryCalls != 1 {
		t.Fatalf("expected exactly one attempt against the failed primary, got %d", primaryCalls)
	}
}

func TestInferenceComplete_NoAccountRegistered_Returns404(t *testing.T) {
	ts := newTestServer(t, true)
	body, _ := json.Marshal(map[string]any{"provider": "nonexistent", "model": "x", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	resp := postJSON(t, ts.URL+"/v1/inference/complete", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for an unregistered provider, got %d", resp.StatusCode)
	}
}

func TestInferenceComplete_OperatorTokenAlsoAllowed(t *testing.T) {
	fakeProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer fakeProvider.Close()

	ts := newTestServer(t, true)
	registerFakeModelProvider(t, ts, fakeProvider.URL)

	body, _ := json.Marshal(map[string]any{"provider": "test-fake", "model": "x", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	resp := postJSON(t, ts.URL+"/v1/inference/complete", testOperatorToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected operator token to also be allowed, got %d", resp.StatusCode)
	}
}

func TestInferenceRoutes_503WhenCredentialStoreDisabled(t *testing.T) {
	ts := newTestServer(t, false)
	body, _ := json.Marshal(map[string]any{"model": "x", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	resp := postJSON(t, ts.URL+"/v1/inference/complete", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the credential store isn't configured, got %d", resp.StatusCode)
	}
}

func TestInferenceComplete_RequiresModel(t *testing.T) {
	ts := newTestServer(t, true)
	body, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": "hi"}}})
	resp := postJSON(t, ts.URL+"/v1/inference/complete", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when model is missing, got %d", resp.StatusCode)
	}
}

func TestInferenceEmbed_RealRoundTripThroughRegisteredProvider(t *testing.T) {
	fakeProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("expected path /embeddings, got %q", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3],"index":0}]}`))
	}))
	defer fakeProvider.Close()

	ts := newTestServer(t, true)
	registerFakeModelProvider(t, ts, fakeProvider.URL)

	body, _ := json.Marshal(map[string]any{"provider": "test-fake", "model": "test-embed-model", "input": []string{"hello world"}})
	resp := postJSON(t, ts.URL+"/v1/inference/embed", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result inferenceEmbedResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Dimension != 3 || len(result.Embeddings) != 1 || result.Embeddings[0][0] != 0.1 {
		t.Fatalf("expected the real provider's embedding, got %+v", result)
	}
}

func TestInferenceEmbed_RequiresModelAndInput(t *testing.T) {
	ts := newTestServer(t, true)

	body, _ := json.Marshal(map[string]any{"input": []string{"hi"}})
	resp := postJSON(t, ts.URL+"/v1/inference/embed", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when model is missing, got %d", resp.StatusCode)
	}

	body2, _ := json.Marshal(map[string]any{"model": "m"})
	resp2 := postJSON(t, ts.URL+"/v1/inference/embed", testAgentToken, body2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when input is missing, got %d", resp2.StatusCode)
	}
}

func TestInferenceEmbed_AnthropicProvider_ReturnsBadRequest(t *testing.T) {
	ts := newTestServer(t, true)
	createBody, _ := json.Marshal(map[string]string{"provider": "anthropic", "display_name": "anthropic"})
	create := postJSON(t, ts.URL+"/v1/accounts", testOperatorToken, createBody)
	var acct accountResponse
	json.NewDecoder(create.Body).Decode(&acct)
	create.Body.Close()
	envelope, _ := json.Marshal(map[string]string{"kind": "anthropic", "api_key": "k"})
	credBody, _ := json.Marshal(map[string]string{"secret": string(envelope)})
	postJSON(t, ts.URL+"/v1/accounts/"+acct.ID+"/credential", testOperatorToken, credBody).Body.Close()

	body, _ := json.Marshal(map[string]any{"provider": "anthropic", "model": "voyage-3", "input": []string{"hi"}})
	resp := postJSON(t, ts.URL+"/v1/inference/embed", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a provider kind with no embeddings support, got %d", resp.StatusCode)
	}
}

// ── OpenAI-compatible facade ─────────────────────────────────────────────

func TestOpenAIChatCompletions_RealRoundTripThroughRegisteredProvider(t *testing.T) {
	var gotAuth string
	fakeProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"the real answer"}}]}`))
	}))
	defer fakeProvider.Close()

	ts := newTestServer(t, true)
	registerFakeModelProvider(t, ts, fakeProvider.URL)

	// "<provider>/<model>" convention: the part before the first "/"
	// selects the registered AMH account, matching litellm/OpenRouter.
	body, _ := json.Marshal(map[string]any{
		"model": "test-fake/test-model",
		"messages": []map[string]string{
			{"role": "system", "content": "be helpful"},
			{"role": "user", "content": "hi"},
		},
	})
	resp := postJSON(t, ts.URL+"/v1/openai/chat/completions", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, mustReadAll(t, resp.Body))
	}
	var result openAIChatCompletionResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if len(result.Choices) != 1 || result.Choices[0].Message.Content != "the real answer" {
		t.Fatalf("expected the real provider text in choices[0], got %+v", result)
	}
	if result.Choices[0].Message.Role != "assistant" || result.Object != "chat.completion" {
		t.Fatalf("expected a real chat.completion-shaped response, got %+v", result)
	}
	if result.ID == "" || result.Created == 0 {
		t.Fatalf("expected a real id and created timestamp, got %+v", result)
	}
	// The fake provider account was registered with api_key "test-key" —
	// proving the facade resolved the AMH-registered credential, not the
	// caller's own agent bearer token, as the provider's Authorization.
	if gotAuth != "Bearer test-key" {
		t.Fatalf("expected the provider to see its registered credential, got %q", gotAuth)
	}
}

func TestOpenAIChatCompletions_BareModelDefaultsProvider(t *testing.T) {
	fakeProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"default provider answer"}}]}`))
	}))
	defer fakeProvider.Close()

	ts := newTestServer(t, true)
	// No "/" in "model": provider defaults the same way
	// inference.Request.Provider's default ("anthropic") already does.
	registerFakeModelProviderNamed(t, ts, "anthropic", fakeProvider.URL)

	body, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	resp := postJSON(t, ts.URL+"/v1/openai/chat/completions", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, mustReadAll(t, resp.Body))
	}
	var result openAIChatCompletionResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Choices[0].Message.Content != "default provider answer" {
		t.Fatalf("expected the default-provider answer, got %+v", result)
	}
}

func TestOpenAIChatCompletions_NoAccountRegistered_ReturnsOpenAIShapedError(t *testing.T) {
	ts := newTestServer(t, true)
	body, _ := json.Marshal(map[string]any{
		"model":    "nonexistent/x",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	resp := postJSON(t, ts.URL+"/v1/openai/chat/completions", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for an unregistered provider, got %d", resp.StatusCode)
	}
	var result openAIErrorResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Error.Message == "" || result.Error.Type == "" {
		t.Fatalf("expected an OpenAI-shaped {error:{message,type}} body, got %+v", result)
	}
}

func TestOpenAIChatCompletions_RequiresModel(t *testing.T) {
	ts := newTestServer(t, true)
	body, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": "hi"}}})
	resp := postJSON(t, ts.URL+"/v1/openai/chat/completions", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when model is missing, got %d", resp.StatusCode)
	}
}

func TestOpenAIEmbeddings_RealRoundTripThroughRegisteredProvider(t *testing.T) {
	fakeProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("expected path /embeddings, got %q", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3],"index":0}]}`))
	}))
	defer fakeProvider.Close()

	ts := newTestServer(t, true)
	registerFakeModelProvider(t, ts, fakeProvider.URL)

	// Real OpenAI embeddings API accepts "input" as a bare string, not
	// just an array — the facade's openAIEmbedInput must accept both.
	body, _ := json.Marshal(map[string]any{"model": "test-fake/test-embed-model", "input": "hello world"})
	resp := postJSON(t, ts.URL+"/v1/openai/embeddings", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, mustReadAll(t, resp.Body))
	}
	var result openAIEmbeddingsResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Object != "list" || len(result.Data) != 1 {
		t.Fatalf("expected a real list-shaped response, got %+v", result)
	}
	if result.Data[0].Object != "embedding" || result.Data[0].Embedding[0] != 0.1 {
		t.Fatalf("expected the real provider's embedding, got %+v", result.Data[0])
	}
}

func TestOpenAIEmbeddings_ArrayInput(t *testing.T) {
	fakeProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&decoded)
		if len(decoded.Input) != 2 {
			t.Errorf("expected both inputs to reach the provider, got %+v", decoded.Input)
		}
		w.Write([]byte(`{"data":[{"embedding":[0.1],"index":0},{"embedding":[0.2],"index":1}]}`))
	}))
	defer fakeProvider.Close()

	ts := newTestServer(t, true)
	registerFakeModelProvider(t, ts, fakeProvider.URL)

	body, _ := json.Marshal(map[string]any{"model": "test-fake/m", "input": []string{"a", "b"}})
	resp := postJSON(t, ts.URL+"/v1/openai/embeddings", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, mustReadAll(t, resp.Body))
	}
	var result openAIEmbeddingsResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if len(result.Data) != 2 {
		t.Fatalf("expected two embeddings back, got %+v", result)
	}
}

func TestOpenAIEmbeddings_RequiresModelAndInput(t *testing.T) {
	ts := newTestServer(t, true)

	body, _ := json.Marshal(map[string]any{"input": "hi"})
	resp := postJSON(t, ts.URL+"/v1/openai/embeddings", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when model is missing, got %d", resp.StatusCode)
	}

	body2, _ := json.Marshal(map[string]any{"model": "test-fake/m"})
	resp2 := postJSON(t, ts.URL+"/v1/openai/embeddings", testAgentToken, body2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when input is missing, got %d", resp2.StatusCode)
	}
}

func TestOpenAIRoutes_503WhenCredentialStoreDisabled(t *testing.T) {
	ts := newTestServer(t, false)
	body, _ := json.Marshal(map[string]any{"model": "x", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	resp := postJSON(t, ts.URL+"/v1/openai/chat/completions", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the credential store isn't configured, got %d", resp.StatusCode)
	}
}

func mustReadAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return b
}
