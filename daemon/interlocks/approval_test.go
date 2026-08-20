package interlocks

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

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

func TestApprovalGateBlocksUntilApproved(t *testing.T) {
	db := testDB(t)
	gate := New(db)
	ctx := context.Background()

	action := map[string]string{"device_action_id": "nutrient-doser.dispense_ml"}
	ticket, err := gate.Require(ctx, action, Irreversible)
	if err != nil {
		t.Fatalf("Require: %v", err)
	}

	if err := gate.Enforce(ctx, ticket); !errors.Is(err, ErrNotSatisfied) {
		t.Fatalf("expected ErrNotSatisfied before approval, got %v", err)
	}

	if err := gate.Approve(ctx, ticket, "operator:jane"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if err := gate.Enforce(ctx, ticket); err != nil {
		t.Fatalf("expected Enforce to pass after approval, got %v", err)
	}
}

func TestApproveIsNotDoubleGrantable(t *testing.T) {
	db := testDB(t)
	gate := New(db)
	ctx := context.Background()

	ticket, err := gate.Require(ctx, map[string]string{"x": "y"}, Irreversible)
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
