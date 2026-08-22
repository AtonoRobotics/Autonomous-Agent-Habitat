package mcp

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
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

func testServer(t *testing.T, db *sql.DB) *httptest.Server {
	t.Helper()
	auth, err := authn.New(testAgentToken, testOperatorToken)
	if err != nil {
		t.Fatalf("authn.New: %v", err)
	}
	tp := sdktrace.NewTracerProvider()
	s := New("", db, tp, auth, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// rpc sends one JSON-RPC request/notification and returns the raw HTTP
// response — callers decode the body themselves, since a notification
// has no JSON body to decode.
func rpc(t *testing.T, ts *httptest.Server, token, sessionID, method string, params any, id any) *http.Response {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		body["params"] = params
	}
	if id != nil {
		body["id"] = id
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		req.Header.Set(sessionIDHeader, sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// initializeSession performs a real initialize call and returns the
// session ID from the response header — the same handshake a real MCP
// client performs before anything else.
func initializeSession(t *testing.T, ts *httptest.Server, token string) string {
	t.Helper()
	resp := rpc(t, ts, token, "", "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]string{"name": "test-client", "version": "1.0"},
	}, 1)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: expected 200, got %d", resp.StatusCode)
	}
	sessionID := resp.Header.Get(sessionIDHeader)
	if sessionID == "" {
		t.Fatalf("initialize: expected a %s response header", sessionIDHeader)
	}
	var decoded response
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode initialize response: %v", err)
	}
	if decoded.Error != nil {
		t.Fatalf("initialize returned an error: %+v", decoded.Error)
	}
	return sessionID
}

func TestInitialize_ReturnsSessionAndProtocolVersion(t *testing.T) {
	ts := testServer(t, testDB(t))
	resp := rpc(t, ts, testAgentToken, "", "initialize", map[string]any{"protocolVersion": "2025-06-18"}, 1)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get(sessionIDHeader) == "" {
		t.Fatalf("expected a %s header", sessionIDHeader)
	}
	var decoded response
	json.NewDecoder(resp.Body).Decode(&decoded)
	result, ok := decoded.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected a result object, got %+v", decoded.Result)
	}
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("expected protocolVersion 2025-06-18, got %v", result["protocolVersion"])
	}
}

func TestPost_RequiresAuth(t *testing.T) {
	ts := testServer(t, testDB(t))
	resp := rpc(t, ts, "", "", "initialize", map[string]any{"protocolVersion": "2025-06-18"}, 1)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d", resp.StatusCode)
	}
}

func TestGet_Returns405(t *testing.T) {
	ts := testServer(t, testDB(t))
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+testAgentToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}
}

func TestToolsList_WithoutSession_Returns400(t *testing.T) {
	ts := testServer(t, testDB(t))
	resp := rpc(t, ts, testAgentToken, "", "tools/list", nil, 1)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 with no session header, got %d", resp.StatusCode)
	}
}

func TestToolsList_WithUnknownSession_Returns404(t *testing.T) {
	ts := testServer(t, testDB(t))
	resp := rpc(t, ts, testAgentToken, "not-a-real-session", "tools/list", nil, 1)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown session, got %d", resp.StatusCode)
	}
}

func TestToolsList_ReturnsTheEmptyCatalog(t *testing.T) {
	ts := testServer(t, testDB(t))
	sessionID := initializeSession(t, ts, testAgentToken)

	resp := rpc(t, ts, testAgentToken, sessionID, "tools/list", nil, 2)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var decoded response
	json.NewDecoder(resp.Body).Decode(&decoded)
	result := decoded.Result.(map[string]any)
	toolList := result["tools"].([]any)
	if len(toolList) != 0 {
		t.Fatalf("expected an empty tool catalog, got %+v", toolList)
	}
}

func TestToolsCall_UnknownTool_ReturnsInvalidParamsError(t *testing.T) {
	ts := testServer(t, testDB(t))
	sessionID := initializeSession(t, ts, testAgentToken)

	resp := rpc(t, ts, testAgentToken, sessionID, "tools/call", map[string]any{"name": "does_not_exist", "arguments": map[string]any{}}, 2)
	defer resp.Body.Close()
	var decoded response
	json.NewDecoder(resp.Body).Decode(&decoded)
	if decoded.Error == nil || decoded.Error.Code != codeInvalidParams {
		t.Fatalf("expected a %d invalid-params error, got %+v", codeInvalidParams, decoded.Error)
	}
}

func TestUnknownMethod_ReturnsMethodNotFoundError(t *testing.T) {
	ts := testServer(t, testDB(t))
	sessionID := initializeSession(t, ts, testAgentToken)

	resp := rpc(t, ts, testAgentToken, sessionID, "prompts/list", nil, 2)
	defer resp.Body.Close()
	var decoded response
	json.NewDecoder(resp.Body).Decode(&decoded)
	if decoded.Error == nil || decoded.Error.Code != codeMethodNotFound {
		t.Fatalf("expected a %d method-not-found error, got %+v", codeMethodNotFound, decoded.Error)
	}
}

func TestNotificationsInitialized_Returns202NoBody(t *testing.T) {
	ts := testServer(t, testDB(t))
	sessionID := initializeSession(t, ts, testAgentToken)

	resp := rpc(t, ts, testAgentToken, sessionID, "notifications/initialized", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}
}
