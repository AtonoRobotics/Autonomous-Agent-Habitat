package interlocks

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

func TestApprovalGateBlocksUntilApproved(t *testing.T) {
	db := testDB(t)
	gate := New(db)
	ctx := context.Background()

	params := map[string]string{"ml": "5"}
	ticket, err := gate.Require(ctx, "nutrient-doser.dispense_ml", params, "test", Irreversible)
	if err != nil {
		t.Fatalf("Require: %v", err)
	}

	if err := gate.Enforce(ctx, ticket, "nutrient-doser.dispense_ml", params); !errors.Is(err, ErrNotSatisfied) {
		t.Fatalf("expected ErrNotSatisfied before approval, got %v", err)
	}

	if err := gate.Approve(ctx, ticket, "operator:jane"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if err := gate.Enforce(ctx, ticket, "nutrient-doser.dispense_ml", params); err != nil {
		t.Fatalf("expected Enforce to pass after approval, got %v", err)
	}
}

func TestApproveIsNotDoubleGrantable(t *testing.T) {
	db := testDB(t)
	gate := New(db)
	ctx := context.Background()

	ticket, err := gate.Require(ctx, "device.action", map[string]string{"x": "y"}, "test", Irreversible)
	if err != nil {
		t.Fatalf("Require: %v", err)
	}
	if err := gate.Approve(ctx, ticket, "operator:jane"); err != nil {
		t.Fatalf("first Approve: %v", err)
	}
	if err := gate.Approve(ctx, ticket, "operator:jane"); err == nil {
		t.Fatalf("expected second Approve on an already-approved ticket to fail")
	}
}

// TestEnforce_RejectsMismatchedAction guards against a real bug: an
// approved ticket used to be checked by ID only, so it could authorize
// any actuation regardless of what it was actually approved for. Enforce
// must refuse a ticket approved for one action when a different action
// (or different params on the same device_action) is what's actually
// being requested.
func TestEnforce_RejectsMismatchedAction(t *testing.T) {
	db := testDB(t)
	gate := New(db)
	ctx := context.Background()

	ticket, err := gate.Require(ctx, "nutrient-doser.dispense_ml", map[string]string{"ml": "5"}, "test", Irreversible)
	if err != nil {
		t.Fatalf("Require: %v", err)
	}
	if err := gate.Approve(ctx, ticket, "operator:jane"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Same ticket, different device_action entirely.
	if err := gate.Enforce(ctx, ticket, "vent-actuator.set_open_pct", map[string]string{"ml": "5"}); !errors.Is(err, ErrActionMismatch) {
		t.Fatalf("expected ErrActionMismatch for a different device_action, got %v", err)
	}

	// Same device_action, different params — must also be refused; an
	// approved "dispense 5ml" ticket must not authorize "dispense 500ml".
	if err := gate.Enforce(ctx, ticket, "nutrient-doser.dispense_ml", map[string]string{"ml": "500"}); !errors.Is(err, ErrActionMismatch) {
		t.Fatalf("expected ErrActionMismatch for different params, got %v", err)
	}

	// The exact original action still succeeds.
	if err := gate.Enforce(ctx, ticket, "nutrient-doser.dispense_ml", map[string]string{"ml": "5"}); err != nil {
		t.Fatalf("expected Enforce to succeed for the exact approved action, got %v", err)
	}
}

// TestEnforce_TicketIsSingleUse guards against the other half of the same
// bug: even for the correct action, an approved ticket must not be
// replayable for a second actuation.
func TestEnforce_TicketIsSingleUse(t *testing.T) {
	db := testDB(t)
	gate := New(db)
	ctx := context.Background()

	params := map[string]string{"ml": "5"}
	ticket, err := gate.Require(ctx, "nutrient-doser.dispense_ml", params, "test", Irreversible)
	if err != nil {
		t.Fatalf("Require: %v", err)
	}
	if err := gate.Approve(ctx, ticket, "operator:jane"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if err := gate.Enforce(ctx, ticket, "nutrient-doser.dispense_ml", params); err != nil {
		t.Fatalf("expected first Enforce to succeed, got %v", err)
	}
	if err := gate.Enforce(ctx, ticket, "nutrient-doser.dispense_ml", params); !errors.Is(err, ErrTicketAlreadyUsed) {
		t.Fatalf("expected ErrTicketAlreadyUsed on replay, got %v", err)
	}
}
