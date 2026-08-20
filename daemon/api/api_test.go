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

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/connectors"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/internal/testssh"
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

	if _, err := db.Exec("INSERT INTO connector (id, type, auth, config) VALUES ('greenhouse-vent', 'ssh', 'none', ?)", string(configJSON)); err != nil {
		t.Fatalf("seed connector: %v", err)
	}
	if _, err := db.Exec("INSERT INTO device (id, kind, connector_id) VALUES ('vent-actuator', 'vent', 'greenhouse-vent')"); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	_, err = db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, inverse_template, verified_at)
		VALUES ('vent-actuator.set_open_pct', 'vent-actuator', 'set_open_pct', 1,
		        '{"shell_template": "vent-ctl set-open-pct {{prior}}"}',
		        strftime('%Y-%m-%dT%H:%M:%fZ','now'))`)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}
	return "vent-actuator.set_open_pct"
}

func TestHandleActuate_RealSSHRoundTripOverHTTP(t *testing.T) {
	db := testDB(t)
	deviceActionID := seedVentDeviceAction(t, db)

	tp := sdktrace.NewTracerProvider() // no exporter needed for this test
	server := New("", db, tp, nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	reqBody, _ := json.Marshal(map[string]string{
		"forward":    "vent-ctl set-open-pct 60",
		"read_state": "vent-ctl get-open-pct",
	})
	resp, err := http.Post(ts.URL+"/v1/device-actions/"+deviceActionID+"/actuate", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
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
	err = db.QueryRow("SELECT inverse_payload FROM device_effect WHERE device_action_id = ?", deviceActionID).Scan(&inverse)
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
	db.Exec("INSERT INTO connector (id, type, auth, config) VALUES ('greenhouse-vent', 'ssh', 'none', ?)", string(configJSON))
	db.Exec("INSERT INTO device (id, kind, connector_id) VALUES ('vent-actuator', 'vent', 'greenhouse-vent')")
	db.Exec(`INSERT INTO device_action (id, device_id, name, reversible) VALUES ('vent-actuator.dispense_ml', 'vent-actuator', 'dispense_ml', 0)`)

	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	reqBody, _ := json.Marshal(map[string]string{"forward": "dose 5ml"})
	resp, err := http.Post(ts.URL+"/v1/device-actions/vent-actuator.dispense_ml/actuate", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden with no ticket for an irreversible action, got %d", resp.StatusCode)
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
