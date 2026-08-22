package actuation

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/interlocks"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store/storetest"
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
	return storetest.Open(t, "../../store/migrations")
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
		(id, device_id, name, reversible, forward_template, read_state_template, inverse_template, verified_at)
		VALUES ($1, 'vent-actuator', 'set_open_pct', 1, $2, $3, $4, iso8601_now())`,
		"vent-actuator.set_open_pct",
		`{"shell_template": "vent-ctl set-open-pct {{open_pct}}"}`,
		`{"shell_template": "vent-ctl get-open-pct"}`,
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
		Params: map[string]string{"open_pct": "60"},
	}, nil /* no ticket needed: verified inverse */)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "ok" {
		t.Fatalf("expected result 'ok', got %q", result)
	}

	var forward, inverse string
	var outcome string
	err = db.QueryRow(`SELECT forward_payload, inverse_payload, outcome FROM device_effect WHERE device_action_id = $1`,
		"vent-actuator.set_open_pct").Scan(&forward, &inverse, &outcome)
	if err != nil {
		t.Fatalf("query device_effect: %v", err)
	}
	if outcome != "success" {
		t.Fatalf("expected outcome success, got %q", outcome)
	}
	if forward != `{"shell":"vent-ctl set-open-pct 60"}` {
		t.Fatalf("expected forward command rendered server-side from the template, got %q", forward)
	}
	if inverse != `{"shell":"vent-ctl set-open-pct 40"}` {
		t.Fatalf("expected inverse built from prior state 40, got %q", inverse)
	}
}

// TestExecute_ParamValueRejectsShellMetacharacters guards against command
// injection through a caller-supplied parameter: since forward/read-state
// commands are now rendered server-side from a template, the only
// attacker-controlled input left is a parameter value — it must never be
// allowed to smuggle in shell syntax the template's author didn't intend.
func TestExecute_ParamValueRejectsShellMetacharacters(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO device_action
		(id, device_id, name, reversible, forward_template, read_state_template, inverse_template, verified_at)
		VALUES ($1, 'vent-actuator', 'set_open_pct', 1, $2, $3, $4, iso8601_now())`,
		"vent-actuator.set_open_pct",
		`{"shell_template": "vent-ctl set-open-pct {{open_pct}}"}`,
		`{"shell_template": "vent-ctl get-open-pct"}`,
		`{"shell_template": "vent-ctl set-open-pct {{prior}}"}`,
	)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}

	act := &fakeActuator{responses: map[string]string{"vent-ctl get-open-pct": "40"}}
	gate := interlocks.New(db)

	_, err = Execute(ctx, db, act, gate, "vent-actuator.set_open_pct", Command{
		Params: map[string]string{"open_pct": "60; rm -rf /"},
	}, nil)
	if !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("expected ErrInvalidParam for a shell-metacharacter param value, got %v", err)
	}
	if len(act.calls) != 0 {
		t.Fatalf("actuator must never be invoked when a param value fails validation, got calls: %v", act.calls)
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
		(id, device_id, name, reversible, forward_template, read_state_template, inverse_template, verified_at)
		VALUES ($1, 'vent-actuator', 'set_open_pct', 1, $2, $3, $4, iso8601_now())`,
		"vent-actuator.set_open_pct",
		`{"shell_template": "vent-ctl set-open-pct {{open_pct}}"}`,
		`{"shell_template": "vent-ctl get-open-pct"}`,
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
		Params: map[string]string{"open_pct": "60"},
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
	if err := db.QueryRow(`SELECT COUNT(*) FROM device_effect WHERE device_action_id = $1`,
		"vent-actuator.set_open_pct").Scan(&count); err != nil {
		t.Fatalf("query device_effect: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no device_effect recorded, got %d", count)
	}
}

// TestExecute_JournalWriteFailureBlocksForwardCommand guards against the
// other real bug PR #3's review found: previously, the device_effect
// journal row was written AFTER the forward command ran, so a DB-write
// failure left an unrecorded physical effect with no idempotency record.
// Now the row is written first, so any DB-write failure on the path to
// running the forward command (the journal write, or consuming the
// ticket) blocks it entirely — proven here by forcing every write to fail
// via a read-only session (Postgres's default_transaction_read_only,
// pinned to one connection so it actually applies to every call Execute
// makes through this *sql.DB).
func TestExecute_JournalWriteFailureBlocksForwardCommand(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	if _, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, forward_template)
		VALUES ('vent-actuator.dispense_ml', 'vent-actuator', 'dispense_ml', 0, $1)`,
		`{"shell_template": "dose {{ml}}ml"}`,
	); err != nil {
		t.Fatalf("seed device_action: %v", err)
	}

	act := &fakeActuator{}
	gate := interlocks.New(db)
	params := map[string]string{"ml": "5"}
	ticket, err := gate.Require(ctx, "vent-actuator.dispense_ml", params, "test", interlocks.Irreversible)
	if err != nil {
		t.Fatalf("Require: %v", err)
	}
	if err := gate.Approve(ctx, ticket, "operator:jane"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	db.SetMaxOpenConns(1) // pin to one connection so the session setting below actually sticks
	if _, err := db.Exec(`SET default_transaction_read_only = ON`); err != nil {
		t.Fatalf("set default_transaction_read_only: %v", err)
	}
	defer db.Exec(`SET default_transaction_read_only = OFF`)

	_, err = Execute(ctx, db, act, gate, "vent-actuator.dispense_ml", Command{Params: params}, &ticket)
	if err == nil {
		t.Fatalf("expected Execute to fail when the device_effect journal write fails")
	}
	if len(act.calls) != 0 {
		t.Fatalf("forward command must never run when the journal write fails, got calls: %v", act.calls)
	}
}

func TestExecuteUnverified_RequiresApprovedTicket(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, forward_template)
		VALUES ('vent-actuator.dispense_ml', 'vent-actuator', 'dispense_ml', 0, $1)`,
		`{"shell_template": "dose {{ml}}ml"}`,
	)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}

	act := &fakeActuator{}
	gate := interlocks.New(db)
	params := map[string]string{"ml": "5"}

	// No ticket at all: must fail closed.
	_, err = Execute(ctx, db, act, gate, "vent-actuator.dispense_ml", Command{Params: params}, nil)
	if err != ErrNoAutonomyPath {
		t.Fatalf("expected ErrNoAutonomyPath with no ticket, got %v", err)
	}

	// Unapproved ticket: must still fail closed.
	ticket, err := gate.Require(ctx, "vent-actuator.dispense_ml", params, "test", interlocks.Irreversible)
	if err != nil {
		t.Fatalf("Require: %v", err)
	}
	_, err = Execute(ctx, db, act, gate, "vent-actuator.dispense_ml", Command{Params: params}, &ticket)
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
	result, err := Execute(ctx, db, act, gate, "vent-actuator.dispense_ml", Command{Params: params}, &ticket)
	if err != nil {
		t.Fatalf("Execute after approval: %v", err)
	}
	_ = result

	if len(act.calls) != 1 || act.calls[0] != "dose 5ml" {
		t.Fatalf("expected the forward command rendered server-side from the template, got calls: %v", act.calls)
	}

	var inverse sql.NullString
	err = db.QueryRow(`SELECT inverse_payload FROM device_effect WHERE device_action_id = $1`,
		"vent-actuator.dispense_ml").Scan(&inverse)
	if err != nil {
		t.Fatalf("query device_effect: %v", err)
	}
	if inverse.Valid {
		t.Fatalf("expected no recorded inverse for an irreversible action, got %q", inverse.String)
	}
}

// TestExecute_TicketCannotAuthorizeADifferentAction guards against the
// exact replay bug PR #3's review found: an approved ticket must not
// authorize an action other than the one it was requested for.
func TestExecute_TicketCannotAuthorizeADifferentAction(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	if _, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, forward_template)
		VALUES ('vent-actuator.dispense_ml', 'vent-actuator', 'dispense_ml', 0, $1)`,
		`{"shell_template": "dose {{ml}}ml"}`,
	); err != nil {
		t.Fatalf("seed device_action: %v", err)
	}

	act := &fakeActuator{}
	gate := interlocks.New(db)

	// Ticket approved for 5ml.
	ticket, err := gate.Require(ctx, "vent-actuator.dispense_ml", map[string]string{"ml": "5"}, "test", interlocks.Irreversible)
	if err != nil {
		t.Fatalf("Require: %v", err)
	}
	if err := gate.Approve(ctx, ticket, "operator:jane"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Attempting to actuate 500ml with the ticket approved for 5ml must fail.
	_, err = Execute(ctx, db, act, gate, "vent-actuator.dispense_ml", Command{Params: map[string]string{"ml": "500"}}, &ticket)
	if !errors.Is(err, interlocks.ErrActionMismatch) {
		t.Fatalf("expected ErrActionMismatch, got %v", err)
	}
	if len(act.calls) != 0 {
		t.Fatalf("actuator must never be invoked for a mismatched action, got calls: %v", act.calls)
	}

	// The actual approved action still succeeds, and consumes the ticket.
	_, err = Execute(ctx, db, act, gate, "vent-actuator.dispense_ml", Command{Params: map[string]string{"ml": "5"}}, &ticket)
	if err != nil {
		t.Fatalf("expected Execute to succeed for the exact approved action, got %v", err)
	}

	// Replaying the same ticket for the same action must now fail too.
	_, err = Execute(ctx, db, act, gate, "vent-actuator.dispense_ml", Command{Params: map[string]string{"ml": "5"}}, &ticket)
	if !errors.Is(err, interlocks.ErrTicketAlreadyUsed) {
		t.Fatalf("expected ErrTicketAlreadyUsed on replay, got %v", err)
	}
}

func TestAutoReverse_UsesRecordedInverse(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, inverse_template, verified_at)
		VALUES ('vent-actuator.set_open_pct', 'vent-actuator', 'set_open_pct', 1, '{}', iso8601_now())`)
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
