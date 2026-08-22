package mcp

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/interlocks"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/internal/testssh"
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

func writeEphemeralClientKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	path := filepath.Join(t.TempDir(), "client_key.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}
	return path
}

func splitHostPort(addr string) (string, int, error) {
	idx := strings.LastIndex(addr, ":")
	host := addr[:idx]
	port, err := strconv.Atoi(addr[idx+1:])
	return host, port, err
}

// newVentHandler simulates a real reversible device: get-open-pct reads
// state, set-open-pct writes it.
func newVentHandler(initial int) testssh.CommandHandler {
	var mu sync.Mutex
	openPct := initial
	return func(cmd string) string {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case cmd == "vent-ctl get-open-pct":
			return strconv.Itoa(openPct)
		case strings.HasPrefix(cmd, "vent-ctl set-open-pct "):
			val, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(cmd, "vent-ctl set-open-pct ")))
			if err != nil {
				return "error: invalid value"
			}
			openPct = val
			return "ok"
		default:
			return "error: unknown command"
		}
	}
}

// seedVentDeviceAction seeds a real reversible+verified device_action
// backed by a real SSH device — the same shape daemon/api's own tests
// use, so a tool call here exercises the identical actuation kernel code
// path, just reached through the MCP endpoint instead of /v1/device-
// actions/.../actuate.
func seedVentDeviceAction(t *testing.T, db *sql.DB) string {
	t.Helper()
	srv := testssh.Start(t, newVentHandler(40))
	host, port, err := splitHostPort(srv.Addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	cfg := map[string]any{
		"host": host, "port": port, "user": "amh",
		"private_key_path":        writeEphemeralClientKey(t),
		"host_key_authorized_key": string(ssh.MarshalAuthorizedKey(srv.HostSigner.PublicKey())),
	}
	configJSON, _ := json.Marshal(cfg)
	if _, err := db.Exec("INSERT INTO connector (id, type, auth, config) VALUES ('greenhouse-vent', 'ssh', 'none', $1)", string(configJSON)); err != nil {
		t.Fatalf("seed connector: %v", err)
	}
	if _, err := db.Exec("INSERT INTO device (id, kind, connector_id) VALUES ('vent-actuator', 'vent', 'greenhouse-vent')"); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	_, err = db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, forward_template, read_state_template, inverse_template, verified_at)
		VALUES ('vent-actuator.set_open_pct', 'vent-actuator', 'set_open_pct', 1, $1, $2, $3, iso8601_now())`,
		`{"shell_template": "vent-ctl set-open-pct {{open_pct}}"}`,
		`{"shell_template": "vent-ctl get-open-pct"}`,
		`{"shell_template": "vent-ctl set-open-pct {{prior}}"}`,
	)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}
	return "vent-actuator.set_open_pct"
}

// seedIrreversibleDeviceAction seeds a device_action with no verified
// inverse — the residue the ApprovalGate exists to cover.
func seedIrreversibleDeviceAction(t *testing.T, db *sql.DB) string {
	t.Helper()
	srv := testssh.Start(t, func(cmd string) string {
		if cmd == "dose 5ml" {
			return "ok"
		}
		return "error: unknown command"
	})
	host, port, err := splitHostPort(srv.Addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	cfg := map[string]any{
		"host": host, "port": port, "user": "amh",
		"private_key_path":        writeEphemeralClientKey(t),
		"host_key_authorized_key": string(ssh.MarshalAuthorizedKey(srv.HostSigner.PublicKey())),
	}
	configJSON, _ := json.Marshal(cfg)
	if _, err := db.Exec("INSERT INTO connector (id, type, auth, config) VALUES ('nutrient-doser-connector', 'ssh', 'none', $1)", string(configJSON)); err != nil {
		t.Fatalf("seed connector: %v", err)
	}
	if _, err := db.Exec("INSERT INTO device (id, kind, connector_id) VALUES ('nutrient-doser', 'doser', 'nutrient-doser-connector')"); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, forward_template)
		VALUES ('nutrient-doser.dispense_ml', 'nutrient-doser', 'dispense_ml', 0, '{"shell_template": "dose {{ml}}ml"}')`); err != nil {
		t.Fatalf("seed device_action: %v", err)
	}
	return "nutrient-doser.dispense_ml"
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

func TestToolsList_ReturnsAllThreeTools(t *testing.T) {
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
	if len(toolList) != 3 {
		t.Fatalf("expected 3 tools, got %d: %+v", len(toolList), toolList)
	}
	names := map[string]bool{}
	for _, raw := range toolList {
		tl := raw.(map[string]any)
		names[tl["name"].(string)] = true
		if _, ok := tl["inputSchema"].(map[string]any); !ok {
			t.Fatalf("expected inputSchema to be a JSON Schema object, got %+v", tl["inputSchema"])
		}
	}
	for _, want := range []string{"actuate_device", "request_approval", "check_approval"} {
		if !names[want] {
			t.Fatalf("expected tool %q in tools/list, got %+v", want, names)
		}
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

func TestToolsCall_ActuateDevice_RealSSHRoundTrip(t *testing.T) {
	db := testDB(t)
	deviceActionID := seedVentDeviceAction(t, db)
	ts := testServer(t, db)
	sessionID := initializeSession(t, ts, testAgentToken)

	resp := rpc(t, ts, testAgentToken, sessionID, "tools/call", map[string]any{
		"name":      "actuate_device",
		"arguments": map[string]any{"device_action_id": deviceActionID, "params": map[string]string{"open_pct": "60"}},
	}, 2)
	defer resp.Body.Close()
	var decoded response
	json.NewDecoder(resp.Body).Decode(&decoded)
	if decoded.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", decoded.Error)
	}
	result := decoded.Result.(map[string]any)
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("expected the tool call to succeed, got isError=true: %+v", result)
	}
	content := result["content"].([]any)[0].(map[string]any)
	if content["text"] != "ok" {
		t.Fatalf("expected the real device response 'ok', got %q", content["text"])
	}

	var inverse string
	if err := db.QueryRow("SELECT inverse_payload FROM device_effect WHERE device_action_id = $1", deviceActionID).Scan(&inverse); err != nil {
		t.Fatalf("query device_effect: %v", err)
	}
	if inverse != `{"shell":"vent-ctl set-open-pct 40"}` {
		t.Fatalf("expected inverse reflecting the real prior state (40), got %q", inverse)
	}
}

// TestApprovalGateLoop_ThroughMCP proves the full "no effect without an
// approved card" loop works through an external MCP client, not just the
// internal Python harness: actuate fails without a ticket, request_
// approval creates one, check_approval reflects its real state, and only
// after a real (non-MCP — approval is deliberately not exposed as a
// tool, see tools.go) operator approval does actuate_device succeed.
func TestApprovalGateLoop_ThroughMCP(t *testing.T) {
	db := testDB(t)
	deviceActionID := seedIrreversibleDeviceAction(t, db)
	ts := testServer(t, db)
	sessionID := initializeSession(t, ts, testAgentToken)

	params := map[string]string{"ml": "5"}

	// 1. No ticket at all: the tool call fails (isError=true), not a
	// protocol error.
	actuateNoTicket := func() map[string]any {
		resp := rpc(t, ts, testAgentToken, sessionID, "tools/call", map[string]any{
			"name":      "actuate_device",
			"arguments": map[string]any{"device_action_id": deviceActionID, "params": params},
		}, 2)
		defer resp.Body.Close()
		var decoded response
		json.NewDecoder(resp.Body).Decode(&decoded)
		if decoded.Error != nil {
			t.Fatalf("unexpected protocol error: %+v", decoded.Error)
		}
		return decoded.Result.(map[string]any)
	}
	result := actuateNoTicket()
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("expected isError=true with no ticket, got %+v", result)
	}

	// 2. request_approval creates a real ticket.
	reqResp := rpc(t, ts, testAgentToken, sessionID, "tools/call", map[string]any{
		"name":      "request_approval",
		"arguments": map[string]any{"device_action_id": deviceActionID, "params": params, "risk": "irreversible", "reason": "test"},
	}, 3)
	defer reqResp.Body.Close()
	var reqDecoded response
	json.NewDecoder(reqResp.Body).Decode(&reqDecoded)
	reqResult := reqDecoded.Result.(map[string]any)
	if isErr, _ := reqResult["isError"].(bool); isErr {
		t.Fatalf("expected request_approval to succeed, got %+v", reqResult)
	}
	var ticketPayload struct {
		TicketID string `json:"ticket_id"`
	}
	text := reqResult["content"].([]any)[0].(map[string]any)["text"].(string)
	if err := json.Unmarshal([]byte(text), &ticketPayload); err != nil {
		t.Fatalf("parse request_approval result: %v", err)
	}
	if ticketPayload.TicketID == "" {
		t.Fatalf("expected a real ticket_id, got %+v", reqResult)
	}

	// 3. check_approval: not yet satisfied.
	checkApproval := func() bool {
		checkResp := rpc(t, ts, testAgentToken, sessionID, "tools/call", map[string]any{
			"name":      "check_approval",
			"arguments": map[string]any{"ticket_id": ticketPayload.TicketID},
		}, 4)
		defer checkResp.Body.Close()
		var checkDecoded response
		json.NewDecoder(checkResp.Body).Decode(&checkDecoded)
		checkResult := checkDecoded.Result.(map[string]any)
		text := checkResult["content"].([]any)[0].(map[string]any)["text"].(string)
		var satisfied struct {
			Satisfied bool `json:"satisfied"`
		}
		json.Unmarshal([]byte(text), &satisfied)
		return satisfied.Satisfied
	}
	if checkApproval() {
		t.Fatalf("expected the ticket to be unsatisfied before approval")
	}

	// 4. Approve it directly via the Gate — approval is deliberately NOT
	// an MCP tool (the same reasoning as agents/workflows/approval.py's
	// module docstring: an agent-reachable surface must never be able to
	// grant its own request).
	gate := interlocks.New(db)
	if err := gate.Approve(t.Context(), interlocks.Ticket{ID: ticketPayload.TicketID}, "operator:jane"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if !checkApproval() {
		t.Fatalf("expected the ticket to be satisfied after approval")
	}

	// 5. Now actuate_device with the approved ticket succeeds.
	actuateResp := rpc(t, ts, testAgentToken, sessionID, "tools/call", map[string]any{
		"name":      "actuate_device",
		"arguments": map[string]any{"device_action_id": deviceActionID, "params": params, "ticket_id": ticketPayload.TicketID},
	}, 5)
	defer actuateResp.Body.Close()
	var actuateDecoded response
	json.NewDecoder(actuateResp.Body).Decode(&actuateDecoded)
	actuateResult := actuateDecoded.Result.(map[string]any)
	if isErr, _ := actuateResult["isError"].(bool); isErr {
		t.Fatalf("expected actuate_device to succeed with the approved ticket, got %+v", actuateResult)
	}
}
