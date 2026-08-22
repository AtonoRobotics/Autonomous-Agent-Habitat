package actuation

import (
	"context"
	"testing"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/interlocks"
)

// These tests cover the SafetyCase autonomy path (§14.7) that
// hasApprovedSafetyCase implements — previously untested here even
// though the query existed, which is exactly how its missing
// independent_review check went unnoticed. See daemon/safetycase for the
// lifecycle that produces these rows.

func TestExecute_ApprovedSafetyCaseGrantsAutonomyWithoutTicket(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, forward_template)
		VALUES ('vent-actuator.dispense_ml', 'vent-actuator', 'dispense_ml', 0, ?)`,
		`{"shell_template": "dose {{ml}}ml"}`,
	)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}
	_, err = db.Exec(`INSERT INTO safety_case
		(id, subject_id, subject_type, risk_class, independent_review, approved_at)
		VALUES ('case-1', 'vent-actuator.dispense_ml', 'device_action', 'high', 1,
		        strftime('%Y-%m-%dT%H:%M:%fZ','now'))`)
	if err != nil {
		t.Fatalf("seed safety_case: %v", err)
	}

	act := &fakeActuator{responses: map[string]string{"dose 5ml": "ok"}}
	gate := interlocks.New(db)

	result, err := Execute(ctx, db, act, gate, "vent-actuator.dispense_ml", Command{Params: map[string]string{"ml": "5"}}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "ok" {
		t.Fatalf("expected result 'ok', got %q", result)
	}

	var inverse *string
	var outcome string
	err = db.QueryRow(`SELECT inverse_payload, outcome FROM device_effect WHERE device_action_id = ?`,
		"vent-actuator.dispense_ml").Scan(&inverse, &outcome)
	if err != nil {
		t.Fatalf("query device_effect: %v", err)
	}
	if outcome != "success" {
		t.Fatalf("expected outcome success, got %q", outcome)
	}
	if inverse != nil {
		t.Fatalf("expected no recorded inverse (nothing to auto-reverse), got %q", *inverse)
	}
}

// The property the independent_review fix exists for: a safety_case row
// with approved_at set but independent_review=0 (e.g. data that reached
// this state some way other than daemon/safetycase.Registry.Approve, or
// a future regression that stops setting both together) must NOT grant
// autonomy for anything above risk_class='low'. Without this check, a
// self-certified case would be indistinguishable from a properly
// independently-reviewed one — exactly the DGM reward-hacking shape §10
// and §14.7 both warn about.
func TestExecute_SafetyCaseWithoutIndependentReviewDoesNotGrantAutonomy(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, forward_template)
		VALUES ('vent-actuator.dispense_ml', 'vent-actuator', 'dispense_ml', 0, ?)`,
		`{"shell_template": "dose {{ml}}ml"}`,
	)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}
	_, err = db.Exec(`INSERT INTO safety_case
		(id, subject_id, subject_type, risk_class, independent_review, approved_at)
		VALUES ('case-1', 'vent-actuator.dispense_ml', 'device_action', 'high', 0,
		        strftime('%Y-%m-%dT%H:%M:%fZ','now'))`)
	if err != nil {
		t.Fatalf("seed safety_case: %v", err)
	}

	act := &fakeActuator{}
	gate := interlocks.New(db)

	_, err = Execute(ctx, db, act, gate, "vent-actuator.dispense_ml", Command{Params: map[string]string{"ml": "5"}}, nil)
	if err != ErrNoAutonomyPath {
		t.Fatalf("expected ErrNoAutonomyPath (independent_review=0 must not grant autonomy), got %v", err)
	}
	if len(act.calls) != 0 {
		t.Fatalf("actuator must not be invoked when the safety_case lacks independent review, got calls: %v", act.calls)
	}
}

// A 'low' risk_class case is the one exception where the spec's floor
// doesn't require independent_review — confirm that carve-out actually
// works, not just the general-case requirement.
func TestExecute_LowRiskSafetyCaseGrantsAutonomyWithoutIndependentReview(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, forward_template)
		VALUES ('vent-actuator.log_status', 'vent-actuator', 'log_status', 0, ?)`,
		`{"shell_template": "log-status"}`,
	)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}
	_, err = db.Exec(`INSERT INTO safety_case
		(id, subject_id, subject_type, risk_class, independent_review, approved_at)
		VALUES ('case-1', 'vent-actuator.log_status', 'device_action', 'low', 0,
		        strftime('%Y-%m-%dT%H:%M:%fZ','now'))`)
	if err != nil {
		t.Fatalf("seed safety_case: %v", err)
	}

	act := &fakeActuator{responses: map[string]string{"log-status": "ok"}}
	gate := interlocks.New(db)

	result, err := Execute(ctx, db, act, gate, "vent-actuator.log_status", Command{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "ok" {
		t.Fatalf("expected result 'ok', got %q", result)
	}
}

func TestExecute_RevokedSafetyCaseDoesNotGrantAutonomy(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, forward_template)
		VALUES ('vent-actuator.dispense_ml', 'vent-actuator', 'dispense_ml', 0, ?)`,
		`{"shell_template": "dose {{ml}}ml"}`,
	)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}
	_, err = db.Exec(`INSERT INTO safety_case
		(id, subject_id, subject_type, risk_class, independent_review, approved_at, revoked_at, revoked_reason)
		VALUES ('case-1', 'vent-actuator.dispense_ml', 'device_action', 'high', 1,
		        strftime('%Y-%m-%dT%H:%M:%fZ','now'), strftime('%Y-%m-%dT%H:%M:%fZ','now'), 'incident')`)
	if err != nil {
		t.Fatalf("seed safety_case: %v", err)
	}

	act := &fakeActuator{}
	gate := interlocks.New(db)

	_, err = Execute(ctx, db, act, gate, "vent-actuator.dispense_ml", Command{Params: map[string]string{"ml": "5"}}, nil)
	if err != ErrNoAutonomyPath {
		t.Fatalf("expected ErrNoAutonomyPath for a revoked safety_case, got %v", err)
	}
	if len(act.calls) != 0 {
		t.Fatalf("actuator must not be invoked when the safety_case is revoked, got calls: %v", act.calls)
	}
}
