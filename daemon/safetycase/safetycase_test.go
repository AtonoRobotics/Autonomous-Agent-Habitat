package safetycase

import (
	"context"
	"database/sql"
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

func TestCreate_RejectsInvalidRiskClassAndSubjectType(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	if _, err := reg.Create(ctx, "nutrient-doser.dispense_ml", SubjectDeviceAction, "not-a-risk"); err != ErrInvalidRiskClass {
		t.Fatalf("expected ErrInvalidRiskClass, got %v", err)
	}
	if _, err := reg.Create(ctx, "nutrient-doser.dispense_ml", "not-a-subject-type", RiskHigh); err != ErrInvalidSubjectType {
		t.Fatalf("expected ErrInvalidSubjectType, got %v", err)
	}
	if _, err := reg.Create(ctx, "nutrient-doser.dispense_ml", SubjectDeviceAction, RiskIrreversibleHighConseq); err != nil {
		t.Fatalf("expected valid creation to succeed, got %v", err)
	}
}

func TestSubmitEvidence_AccumulatesAcrossCalls(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	id, err := reg.Create(ctx, "nutrient-doser.dispense_ml", SubjectDeviceAction, RiskHigh)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := reg.SubmitEvidence(ctx, id, map[string]any{"guardrail": "flow_rate_limiter", "proven": true}); err != nil {
		t.Fatalf("SubmitEvidence 1: %v", err)
	}
	if err := reg.SubmitEvidence(ctx, id, map[string]any{"guardrail": "max_daily_dose", "proven": true}); err != nil {
		t.Fatalf("SubmitEvidence 2: %v", err)
	}

	var guardrailsJSON string
	err = db.QueryRow("SELECT guardrails FROM safety_case WHERE id = ?", id).Scan(&guardrailsJSON)
	if err != nil {
		t.Fatalf("query guardrails: %v", err)
	}
	if guardrailsJSON != `[{"guardrail":"flow_rate_limiter","proven":true},{"guardrail":"max_daily_dose","proven":true}]` {
		t.Fatalf("expected both entries accumulated, got %s", guardrailsJSON)
	}
}

func TestSubmitEvidence_NotFound(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	if err := reg.SubmitEvidence(context.Background(), "does-not-exist", map[string]any{}); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestApprove_SetsIndependentReviewAtomicallyWithApproval(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	id, err := reg.Create(ctx, "nutrient-doser.dispense_ml", SubjectDeviceAction, RiskHigh)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	status, err := reg.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Approved || status.IndependentReview {
		t.Fatalf("expected a fresh case to be neither approved nor independently reviewed")
	}

	if err := reg.Approve(ctx, id, "operator:jane"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	status, err = reg.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status after approve: %v", err)
	}
	if !status.Approved || !status.IndependentReview {
		t.Fatalf("expected both approved and independent_review to be true, got approved=%v independent_review=%v", status.Approved, status.IndependentReview)
	}
}

func TestApprove_RejectsDoubleApproval(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	id, _ := reg.Create(ctx, "x", SubjectDeviceAction, RiskLow)
	if err := reg.Approve(ctx, id, "operator:jane"); err != nil {
		t.Fatalf("first Approve: %v", err)
	}
	if err := reg.Approve(ctx, id, "operator:jane"); err != ErrAlreadyApproved {
		t.Fatalf("expected ErrAlreadyApproved, got %v", err)
	}
}

func TestRevoke_RequiresPriorApproval(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	id, _ := reg.Create(ctx, "x", SubjectDeviceAction, RiskLow)
	if err := reg.Revoke(ctx, id, "never approved"); err != ErrNotApproved {
		t.Fatalf("expected ErrNotApproved, got %v", err)
	}
}

func TestRevoke_IsImmediateAndFinal(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	id, _ := reg.Create(ctx, "x", SubjectDeviceAction, RiskHigh)
	if err := reg.Approve(ctx, id, "operator:jane"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := reg.Revoke(ctx, id, "guardrail failed in the field"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	status, err := reg.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Revoked || status.RevokedReason != "guardrail failed in the field" {
		t.Fatalf("expected revoked=true with reason recorded, got %+v", status)
	}

	// Revoked is final: neither re-approve nor re-revoke works. Per
	// §14.7, the case is rebuilt (a fresh Create), not resurrected.
	if err := reg.Approve(ctx, id, "operator:jane"); err != ErrAlreadyRevoked {
		t.Fatalf("expected re-approving a revoked case to fail with ErrAlreadyRevoked, got %v", err)
	}
	if err := reg.Revoke(ctx, id, "again"); err != ErrAlreadyRevoked {
		t.Fatalf("expected re-revoking to fail with ErrAlreadyRevoked, got %v", err)
	}
}

func TestStatus_NotFound(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	if _, err := reg.Status(context.Background(), "does-not-exist"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
