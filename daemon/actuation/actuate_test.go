package actuation

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/interlocks"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store"
)

// fakeActuator records every shell command it was asked to run and returns
// a scripted response, so tests can assert on exactly what the actuation
// kernel sent without a real SSH transport.
type fakeActuator struct {
	responses map[string]string
	calls     []string
}

func (f *fakeActuator) RunShell(ctx context.Context, command string) (string, error) {
	f.calls = append(f.calls, command)
	if r, ok := f.responses[command]; ok {
		return r, nil
	}
	return "", nil
}

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

func seedConnectorAndDevice(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO connector (id, type, auth) VALUES ('greenhouse-vent', 'ssh', 'apikey')`); err != nil {
		t.Fatalf("seed connector: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO device (id, kind, connector_id) VALUES ('vent-actuator', 'vent', 'greenhouse-vent')`); err != nil {
		t.Fatalf("seed device: %v", err)
	}
}

func TestExecuteReversibleVerified_NoGateNeeded(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO device_action
		(id, device_id, name, reversible, inverse_template, verified_at)
		VALUES (?, 'vent-actuator', 'set_open_pct', 1, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		"vent-actuator.set_open_pct",
		`{"shell_template": "vent-ctl set-open-pct {{prior}}"}`,
	)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}

	act := &fakeActuator{responses: map[string]string{
		"vent-ctl get-open-pct":    "40",
		"vent-ctl set-open-pct 60": "ok",
	}}
	gate := interlocks.New(db)

	result, err := Execute(ctx, db, act, gate, "vent-actuator.set_open_pct", Command{
		Forward:   "vent-ctl set-open-pct 60",
		ReadState: "vent-ctl get-open-pct",
	}, nil /* no ticket needed: verified inverse */)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "ok" {
		t.Fatalf("expected result 'ok', got %q", result)
	}

	var forward, inverse string
	var outcome string
	err = db.QueryRow(`SELECT forward_payload, inverse_payload, outcome FROM device_effect WHERE device_action_id = ?`,
		"vent-actuator.set_open_pct").Scan(&forward, &inverse, &outcome)
	if err != nil {
		t.Fatalf("query device_effect: %v", err)
	}
	if outcome != "success" {
		t.Fatalf("expected outcome success, got %q", outcome)
	}
	if inverse != `{"shell":"vent-ctl set-open-pct 40"}` {
		t.Fatalf("expected inverse built from prior state 40, got %q", inverse)
	}
}

// TestExecuteReversibleVerified_MalformedInverseTemplateNeverInvokesForward
// guards against a real bug: a malformed or missing inverse_template must be
// caught while rendering the inverse from the just-read prior state, before
// the forward command ever runs — not after. Otherwise a bad inverse
// template turns a "reversible, verified" action into an actual physical
// effect with no recorded inverse and no error a caller can act on before
// it's too late.
func TestExecuteReversibleVerified_MalformedInverseTemplateNeverInvokesForward(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO device_action
		(id, device_id, name, reversible, inverse_template, verified_at)
		VALUES (?, 'vent-actuator', 'set_open_pct', 1, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		"vent-actuator.set_open_pct",
		`{"not_shell_template": "oops"}`, // missing the required shell_template field
	)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}

	act := &fakeActuator{responses: map[string]string{
		"vent-ctl get-open-pct":    "40",
		"vent-ctl set-open-pct 60": "ok",
	}}
	gate := interlocks.New(db)

	_, err = Execute(ctx, db, act, gate, "vent-actuator.set_open_pct", Command{
		Forward:   "vent-ctl set-open-pct 60",
		ReadState: "vent-ctl get-open-pct",
	}, nil)
	if err == nil {
		t.Fatalf("expected Execute to fail on a malformed inverse_template")
	}
	for _, call := range act.calls {
		if call == "vent-ctl set-open-pct 60" {
			t.Fatalf("forward command must never run when the inverse template fails to render, but got calls: %v", act.calls)
		}
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM device_effect WHERE device_action_id = ?`,
		"vent-actuator.set_open_pct").Scan(&count); err != nil {
		t.Fatalf("query device_effect: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no device_effect recorded, got %d", count)
	}
}

func TestExecuteUnverified_RequiresApprovedTicket(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible)
		VALUES ('vent-actuator.dispense_ml', 'vent-actuator', 'dispense_ml', 0)`)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}

	act := &fakeActuator{}
	gate := interlocks.New(db)

	// No ticket at all: must fail closed.
	_, err = Execute(ctx, db, act, gate, "vent-actuator.dispense_ml", Command{Forward: "dose 5ml"}, nil)
	if err != ErrNoAutonomyPath {
		t.Fatalf("expected ErrNoAutonomyPath with no ticket, got %v", err)
	}

	// Unapproved ticket: must still fail closed.
	ticket, err := gate.Require(ctx, map[string]string{"action": "dispense_ml"}, interlocks.Irreversible)
	if err != nil {
		t.Fatalf("Require: %v", err)
	}
	_, err = Execute(ctx, db, act, gate, "vent-actuator.dispense_ml", Command{Forward: "dose 5ml"}, &ticket)
	if err == nil {
		t.Fatalf("expected Execute to fail with an unapproved ticket")
	}
	if len(act.calls) != 0 {
		t.Fatalf("actuator must not be invoked before approval, got calls: %v", act.calls)
	}

	// Approved ticket: now it proceeds, and the effect records no inverse.
	if err := gate.Approve(ctx, ticket, "operator:jane"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	result, err := Execute(ctx, db, act, gate, "vent-actuator.dispense_ml", Command{Forward: "dose 5ml"}, &ticket)
	if err != nil {
		t.Fatalf("Execute after approval: %v", err)
	}
	_ = result

	var inverse sql.NullString
	err = db.QueryRow(`SELECT inverse_payload FROM device_effect WHERE device_action_id = ?`,
		"vent-actuator.dispense_ml").Scan(&inverse)
	if err != nil {
		t.Fatalf("query device_effect: %v", err)
	}
	if inverse.Valid {
		t.Fatalf("expected no recorded inverse for an irreversible action, got %q", inverse.String)
	}
}

func TestAutoReverse_UsesRecordedInverse(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, inverse_template, verified_at)
		VALUES ('vent-actuator.set_open_pct', 'vent-actuator', 'set_open_pct', 1, '{}', strftime('%Y-%m-%dT%H:%M:%fZ','now'))`)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}
	_, err = db.Exec(`INSERT INTO device_effect (id, device_action_id, forward_payload, inverse_payload, outcome)
		VALUES ('effect-1', 'vent-actuator.set_open_pct', '{"shell":"vent-ctl set-open-pct 60"}', '{"shell":"vent-ctl set-open-pct 40"}', 'success')`)
	if err != nil {
		t.Fatalf("seed device_effect: %v", err)
	}

	act := &fakeActuator{responses: map[string]string{"vent-ctl set-open-pct 40": "ok"}}
	if err := AutoReverse(ctx, db, act, "effect-1"); err != nil {
		t.Fatalf("AutoReverse: %v", err)
	}

	if len(act.calls) != 1 || act.calls[0] != "vent-ctl set-open-pct 40" {
		t.Fatalf("expected the recorded inverse to be run, got calls: %v", act.calls)
	}

	var outcome string
	var reversedAt sql.NullString
	err = db.QueryRow(`SELECT outcome, reversed_at FROM device_effect WHERE id = 'effect-1'`).Scan(&outcome, &reversedAt)
	if err != nil {
		t.Fatalf("query device_effect: %v", err)
	}
	if outcome != "fault_reversed" || !reversedAt.Valid {
		t.Fatalf("expected outcome fault_reversed with reversed_at set, got outcome=%q reversed_at.Valid=%v", outcome, reversedAt.Valid)
	}
}

func TestAutoReverse_RefusesWhenNoInverseRecorded(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible)
		VALUES ('vent-actuator.dispense_ml', 'vent-actuator', 'dispense_ml', 0)`)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}
	_, err = db.Exec(`INSERT INTO device_effect (id, device_action_id, forward_payload, inverse_payload, outcome)
		VALUES ('effect-2', 'vent-actuator.dispense_ml', '{"shell":"dose 5ml"}', NULL, 'success')`)
	if err != nil {
		t.Fatalf("seed device_effect: %v", err)
	}

	act := &fakeActuator{}
	err = AutoReverse(ctx, db, act, "effect-2")
	if err == nil {
		t.Fatalf("expected AutoReverse to refuse an effect with no recorded inverse")
	}
	if len(act.calls) != 0 {
		t.Fatalf("actuator must not be invoked when there is no inverse to run")
	}
}
