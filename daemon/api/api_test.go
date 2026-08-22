package api

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
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/connectors"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/internal/testssh"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store/storetest"
)

// Fixed test-only tokens — never real secrets, just distinct strings so
// tests can assert agent-vs-operator behavior deterministically.
const (
	testAgentToken    = "test-agent-token"
	testOperatorToken = "test-operator-token"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return storetest.Open(t, "../../store/migrations")
}

func testAuth(t *testing.T) *authn.Authenticator {
	t.Helper()
	auth, err := authn.New(testAgentToken, testOperatorToken)
	if err != nil {
		t.Fatalf("authn.New: %v", err)
	}
	return auth
}

// postJSON and getJSON send an authenticated request with the given
// bearer token — "" sends no Authorization header at all, for testing
// the unauthenticated-request path.
func postJSON(t *testing.T, url, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func getJSON(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// writeEphemeralClientKey generates an RSA key and writes it as a PEM file,
// returning the path — the shape connector.config.private_key_path expects.
func writeEphemeralClientKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	path := filepath.Join(t.TempDir(), "client_key.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}
	return path
}

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

func seedVentDeviceAction(t *testing.T, db *sql.DB, device ...string) string {
	t.Helper()
	srv := testssh.Start(t, newVentHandler(40))
	host, port, err := splitHostPort(srv.Addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}

	clientKeyPath := writeEphemeralClientKey(t)
	cfg := connectors.SSHConnectorConfig{
		Host:                 host,
		Port:                 port,
		User:                 "amh",
		PrivateKeyPath:       clientKeyPath,
		HostKeyAuthorizedKey: string(ssh.MarshalAuthorizedKey(srv.HostSigner.PublicKey())),
	}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	if _, err := db.Exec("INSERT INTO connector (id, type, auth, config) VALUES ('greenhouse-vent', 'ssh', 'none', $1)", string(configJSON)); err != nil {
		t.Fatalf("seed connector: %v", err)
	}
	if _, err := db.Exec("INSERT INTO device (id, kind, connector_id) VALUES ('vent-actuator', 'vent', 'greenhouse-vent')"); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	_, err = db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, forward_template, read_state_template, inverse_template, verified_at)
		VALUES ('vent-actuator.set_open_pct', 'vent-actuator', 'set_open_pct', 1,
		        '{"shell_template": "vent-ctl set-open-pct {{open_pct}}"}',
		        '{"shell_template": "vent-ctl get-open-pct"}',
		        '{"shell_template": "vent-ctl set-open-pct {{prior}}"}',
		        iso8601_now())`)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}
	return "vent-actuator.set_open_pct"
}

// seedIrreversibleDeviceAction sets up a device_action with no verified
// inverse (reversible=0) — the residue the ApprovalGate exists to cover —
// backed by a real reachable SSH device, so the "now it proceeds" leg of
// TestIrreversibleActuation_RequiresApprovalCreatedAndApprovedOverHTTP can
// genuinely execute the forward command over SSH once approved.
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

	cfg := connectors.SSHConnectorConfig{
		Host:                 host,
		Port:                 port,
		User:                 "amh",
		PrivateKeyPath:       writeEphemeralClientKey(t),
		HostKeyAuthorizedKey: string(ssh.MarshalAuthorizedKey(srv.HostSigner.PublicKey())),
	}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

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

func TestHandleActuate_RealSSHRoundTripOverHTTP(t *testing.T) {
	db := testDB(t)
	deviceActionID := seedVentDeviceAction(t, db)

	tp := sdktrace.NewTracerProvider() // no exporter needed for this test
	server := New("", db, tp, testAuth(t), nil, t.TempDir(), nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	reqBody, _ := json.Marshal(map[string]any{
		"params": map[string]string{"open_pct": "60"},
	})
	resp := postJSON(t, ts.URL+"/v1/device-actions/"+deviceActionID+"/actuate", testAgentToken, reqBody)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body actuateResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Result != "ok" {
		t.Fatalf("expected result 'ok', got %q (error: %q)", body.Result, body.Error)
	}

	var inverse string
	err := db.QueryRow("SELECT inverse_payload FROM device_effect WHERE device_action_id = $1", deviceActionID).Scan(&inverse)
	if err != nil {
		t.Fatalf("query device_effect: %v", err)
	}
	if inverse != `{"shell":"vent-ctl set-open-pct 40"}` {
		t.Fatalf("expected inverse reflecting the real prior state (40), got %q", inverse)
	}
}

func TestHandleActuate_UnreversibleWithoutTicketIsForbidden(t *testing.T) {
	db := testDB(t)
	srv := testssh.Start(t, newVentHandler(0))
	host, port, err := splitHostPort(srv.Addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	clientKeyPath := writeEphemeralClientKey(t)
	cfg := connectors.SSHConnectorConfig{
		Host: host, Port: port, User: "amh",
		PrivateKeyPath:       clientKeyPath,
		HostKeyAuthorizedKey: string(ssh.MarshalAuthorizedKey(srv.HostSigner.PublicKey())),
	}
	configJSON, _ := json.Marshal(cfg)
	db.Exec("INSERT INTO connector (id, type, auth, config) VALUES ('greenhouse-vent', 'ssh', 'none', $1)", string(configJSON))
	db.Exec("INSERT INTO device (id, kind, connector_id) VALUES ('vent-actuator', 'vent', 'greenhouse-vent')")
	db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, forward_template) VALUES ('vent-actuator.dispense_ml', 'vent-actuator', 'dispense_ml', 0, '{"shell_template": "dose {{ml}}ml"}')`)

	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, testAuth(t), nil, t.TempDir(), nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	reqBody, _ := json.Marshal(map[string]any{"params": map[string]string{"ml": "5"}})
	resp := postJSON(t, ts.URL+"/v1/device-actions/vent-actuator.dispense_ml/actuate", testAgentToken, reqBody)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden with no ticket for an irreversible action, got %d", resp.StatusCode)
	}
}

func TestHandleActuate_RejectsRequestsWithNoToken(t *testing.T) {
	db := testDB(t)
	deviceActionID := seedVentDeviceAction(t, db)

	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, testAuth(t), nil, t.TempDir(), nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	reqBody, _ := json.Marshal(map[string]any{"params": map[string]string{"open_pct": "60"}})
	resp := postJSON(t, ts.URL+"/v1/device-actions/"+deviceActionID+"/actuate", "", reqBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no Authorization header, got %d", resp.StatusCode)
	}
}

// splitHostPort avoids pulling in net just for this in a test file already
// importing plenty; kept trivial and local.
func splitHostPort(addr string) (string, int, error) {
	idx := strings.LastIndex(addr, ":")
	host := addr[:idx]
	port, err := strconv.Atoi(addr[idx+1:])
	return host, port, err
}
