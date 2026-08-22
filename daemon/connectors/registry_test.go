package connectors

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store/storetest"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return storetest.Open(t, "../../store/migrations")
}

// TestResolveActuator_RejectsDisabledConnector guards against a real bug:
// the operator-only disable route (daemon/api's connector disable handler)
// sets connector.status = 'disabled', but ResolveActuator used to ignore
// that column entirely — any actuation for a device on a disabled connector
// still resolved and ran. Disabling a connector must actually revoke its
// use, not just be advisory metadata.
func TestResolveActuator_RejectsDisabledConnector(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, err := db.Exec(`INSERT INTO connector (id, type, auth, config, status) VALUES ('greenhouse-vent', 'ssh', 'apikey', '{}', 'disabled')`); err != nil {
		t.Fatalf("seed connector: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO device (id, kind, connector_id) VALUES ('vent-actuator', 'vent', 'greenhouse-vent')`); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible) VALUES ('vent-actuator.set_open_pct', 'vent-actuator', 'set_open_pct', 0)`); err != nil {
		t.Fatalf("seed device_action: %v", err)
	}

	registry := NewRegistry(db)
	_, err := registry.ResolveActuator(ctx, "vent-actuator.set_open_pct")
	if err == nil {
		t.Fatalf("expected ResolveActuator to refuse a disabled connector")
	}
	if !errors.Is(err, ErrConnectorDisabled) {
		t.Fatalf("expected ErrConnectorDisabled, got %v", err)
	}
}

// TestResolveActuator_AllowsActiveConnector is the control: an active
// connector must still resolve (this exercises the same query path as the
// disabled case, minus the status check, so a regression that starts
// rejecting active connectors too would be caught here).
func TestResolveActuator_AllowsActiveConnector(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, err := db.Exec(`INSERT INTO connector (id, type, auth, config) VALUES ('greenhouse-vent', 'ssh', 'apikey', $1)`,
		`{"host":"127.0.0.1","port":22,"user":"root","private_key_path":"/nonexistent","insecure_skip_host_key_verify":true}`,
	); err != nil {
		t.Fatalf("seed connector: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO device (id, kind, connector_id) VALUES ('vent-actuator', 'vent', 'greenhouse-vent')`); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible) VALUES ('vent-actuator.set_open_pct', 'vent-actuator', 'set_open_pct', 0)`); err != nil {
		t.Fatalf("seed device_action: %v", err)
	}

	registry := NewRegistry(db)
	_, err := registry.ResolveActuator(ctx, "vent-actuator.set_open_pct")
	// The connector is active, so resolution proceeds past the status
	// check and fails later, for an unrelated reason (no real private key
	// at /nonexistent) — that failure, not ErrConnectorDisabled, is what
	// proves the status check isn't what's blocking an active connector.
	if err == nil {
		t.Fatalf("expected a load-signer error from the fake key path, got success")
	}
	if errors.Is(err, ErrConnectorDisabled) {
		t.Fatalf("an active connector must never be rejected as disabled")
	}
}
