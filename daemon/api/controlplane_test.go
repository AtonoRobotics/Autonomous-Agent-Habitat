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
	server := New("", db, tp, testAuth(t), nil, t.TempDir(), creds)
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

// ── Connectors ───────────────────────────────────────────────────────────

func TestConnectorLifecycle_CreateListDisable_OperatorOnly(t *testing.T) {
	ts := newTestServer(t, false)

	createBody, _ := json.Marshal(map[string]any{
		"id":   "my-rest-connector",
		"type": "rest",
		"auth": "apikey",
	})
	agentAttempt := postJSON(t, ts.URL+"/v1/connectors", testAgentToken, createBody)
	agentAttempt.Body.Close()
	if agentAttempt.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for an agent token creating a connector, got %d", agentAttempt.StatusCode)
	}

	create := postJSON(t, ts.URL+"/v1/connectors", testOperatorToken, createBody)
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", create.StatusCode)
	}
	create.Body.Close()

	// A brand new connector type not in 0001's old fixed enum — this is
	// the actual regression test for the migration dropping that CHECK.
	customBody, _ := json.Marshal(map[string]any{
		"id":   "my-custom-connector",
		"type": "amh.test/custom-protocol",
		"auth": "none",
	})
	custom := postJSON(t, ts.URL+"/v1/connectors", testOperatorToken, customBody)
	if custom.StatusCode != http.StatusCreated {
		t.Fatalf("expected a non-enumerated connector type to be accepted, got %d", custom.StatusCode)
	}
	custom.Body.Close()

	list := getJSON(t, ts.URL+"/v1/connectors", testAgentToken)
	var all []connectorResponse
	json.NewDecoder(list.Body).Decode(&all)
	list.Body.Close()
	if len(all) != 2 {
		t.Fatalf("expected 2 connectors listed, got %d", len(all))
	}

	disable := postJSON(t, ts.URL+"/v1/connectors/my-rest-connector/disable", testOperatorToken, nil)
	if disable.StatusCode != http.StatusOK {
		t.Fatalf("disable: expected 200, got %d", disable.StatusCode)
	}
	disable.Body.Close()

	get := getJSON(t, ts.URL+"/v1/connectors/my-rest-connector", testAgentToken)
	var got connectorResponse
	json.NewDecoder(get.Body).Decode(&got)
	get.Body.Close()
	if got.Status != "disabled" {
		t.Fatalf("expected disabled, got %s", got.Status)
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
	createBody, _ := json.Marshal(map[string]string{"provider": "test-fake", "display_name": "fake provider"})
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

func mustReadAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return b
}
