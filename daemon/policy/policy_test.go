package policy

import (
	"context"
	"database/sql"
	"testing"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store/storetest"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return storetest.Open(t, "../../store/migrations")
}

func TestDecide_VerifiedReversibility_Admits(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()

	d, err := e.Decide(ctx, DecideRequest{
		OperationID:   "op-1",
		Payload:       map[string]any{"open_pct": 60},
		Reversibility: ReversibilityVerified,
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Result != ResultAdmit {
		t.Fatalf("expected admit, got %s", d.Result)
	}
	if d.ApprovalRequestID != "" {
		t.Fatalf("expected no approval request for an admitted decision, got %q", d.ApprovalRequestID)
	}
	if d.PolicyID != DefaultPolicyID || d.PolicyVersion != DefaultPolicyVersion {
		t.Fatalf("expected the decision bound to the default policy identity, got %s@%s", d.PolicyID, d.PolicyVersion)
	}
}

func TestDecide_UnverifiedReversibility_NeedsApproval(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()

	for _, rev := range []Reversibility{ReversibilityClaimed, ReversibilityNone, ""} {
		d, err := e.Decide(ctx, DecideRequest{OperationID: "op-1", Payload: map[string]any{"x": 1}, Reversibility: rev})
		if err != nil {
			t.Fatalf("Decide(%q): %v", rev, err)
		}
		if d.Result != ResultNeedsApproval {
			t.Fatalf("Decide(%q): expected needs_approval, got %s", rev, d.Result)
		}
		if d.ApprovalRequestID == "" {
			t.Fatalf("Decide(%q): expected a bound approval request", rev)
		}
		ar, err := e.GetApprovalRequest(ctx, d.ApprovalRequestID)
		if err != nil {
			t.Fatalf("GetApprovalRequest: %v", err)
		}
		if ar.Status != "pending" {
			t.Fatalf("expected a fresh approval request to be pending, got %s", ar.Status)
		}
	}
}

func TestDecide_SamePayload_SameDigest(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()
	payload := map[string]any{"a": 1, "b": "two"}

	d1, err := e.Decide(ctx, DecideRequest{OperationID: "op-1", Payload: payload, Reversibility: ReversibilityVerified})
	if err != nil {
		t.Fatalf("Decide 1: %v", err)
	}
	d2, err := e.Decide(ctx, DecideRequest{OperationID: "op-2", Payload: payload, Reversibility: ReversibilityVerified})
	if err != nil {
		t.Fatalf("Decide 2: %v", err)
	}
	if d1.ActionDigest != d2.ActionDigest {
		t.Fatalf("expected the same payload to produce the same digest, got %q vs %q", d1.ActionDigest, d2.ActionDigest)
	}
	wantDigest, err := Digest(payload)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if d1.ActionDigest != wantDigest {
		t.Fatalf("expected the decision's action_digest to equal Digest(payload), got %q want %q", d1.ActionDigest, wantDigest)
	}
}

func TestConsume_AdmittedDecision_SucceedsOnceThenFails(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()

	d, err := e.Decide(ctx, DecideRequest{OperationID: "op-1", Payload: map[string]any{"x": 1}, Reversibility: ReversibilityVerified})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if err := e.Consume(ctx, d.ID, d.ActionDigest); err != nil {
		t.Fatalf("first Consume: %v", err)
	}
	if err := e.Consume(ctx, d.ID, d.ActionDigest); err == nil {
		t.Fatalf("expected the second Consume of the same decision to fail")
	} else if err != ErrAlreadyConsumed {
		t.Fatalf("expected ErrAlreadyConsumed, got %v", err)
	}
}

func TestConsume_WrongDigest_Fails(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()

	d, err := e.Decide(ctx, DecideRequest{OperationID: "op-1", Payload: map[string]any{"x": 1}, Reversibility: ReversibilityVerified})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	wrongDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if err := e.Consume(ctx, d.ID, wrongDigest); err != ErrDigestMismatch {
		t.Fatalf("expected ErrDigestMismatch, got %v", err)
	}
}

func TestConsume_NeedsApprovalDecision_Fails(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()

	d, err := e.Decide(ctx, DecideRequest{OperationID: "op-1", Payload: map[string]any{"x": 1}, Reversibility: ReversibilityNone})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if err := e.Consume(ctx, d.ID, d.ActionDigest); err != ErrNotAdmitted {
		t.Fatalf("expected ErrNotAdmitted for a needs_approval decision, got %v", err)
	}
}

func TestConsume_UnknownDecision_Fails(t *testing.T) {
	e := New(testDB(t))
	if err := e.Consume(context.Background(), "does-not-exist", "sha256:x"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestConsume_ExpiredDecision_Fails(t *testing.T) {
	db := testDB(t)
	e := New(db)
	ctx := context.Background()

	d, err := e.Decide(ctx, DecideRequest{OperationID: "op-1", Payload: map[string]any{"x": 1}, Reversibility: ReversibilityVerified})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, err := db.Exec(`UPDATE policy_decision SET expires_at = '2000-01-01T00:00:00.000Z' WHERE id = $1`, d.ID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}
	if err := e.Consume(ctx, d.ID, d.ActionDigest); err != ErrDecisionExpired {
		t.Fatalf("expected ErrDecisionExpired, got %v", err)
	}
}

func TestApprove_MintsFreshAdmitDecision_BoundToSameOperationAndDigest(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()

	original, err := e.Decide(ctx, DecideRequest{OperationID: "op-1", Payload: map[string]any{"x": 1}, Reversibility: ReversibilityClaimed})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	approved, err := e.Approve(ctx, original.ApprovalRequestID, "operator:jane")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.ID == original.ID {
		t.Fatalf("expected Approve to mint a NEW decision, not reuse %s", original.ID)
	}
	if approved.Result != ResultAdmit {
		t.Fatalf("expected the minted decision to admit, got %s", approved.Result)
	}
	if approved.OperationID != original.OperationID || approved.ActionDigest != original.ActionDigest {
		t.Fatalf("expected the minted decision bound to the same operation/digest, got %+v vs %+v", approved, original)
	}

	// The original decision is untouched — still needs_approval.
	stillOriginal, err := e.Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("Get original: %v", err)
	}
	if stillOriginal.Result != ResultNeedsApproval {
		t.Fatalf("expected the original decision's result to remain needs_approval, got %s", stillOriginal.Result)
	}

	// The freshly-minted decision is consumable.
	if err := e.Consume(ctx, approved.ID, approved.ActionDigest); err != nil {
		t.Fatalf("Consume the approved decision: %v", err)
	}
}

func TestApprove_AlreadyResolved_Fails(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()

	d, err := e.Decide(ctx, DecideRequest{OperationID: "op-1", Payload: map[string]any{"x": 1}, Reversibility: ReversibilityNone})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, err := e.Approve(ctx, d.ApprovalRequestID, "operator:jane"); err != nil {
		t.Fatalf("first Approve: %v", err)
	}
	if _, err := e.Approve(ctx, d.ApprovalRequestID, "operator:jane"); err != ErrNotPending {
		t.Fatalf("expected ErrNotPending on a second Approve, got %v", err)
	}
}

func TestDeny_MintsNoDecision(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()

	d, err := e.Decide(ctx, DecideRequest{OperationID: "op-1", Payload: map[string]any{"x": 1}, Reversibility: ReversibilityNone})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	ar, err := e.Deny(ctx, d.ApprovalRequestID, "operator:jane", "too risky")
	if err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if ar.Status != "denied" {
		t.Fatalf("expected denied, got %s", ar.Status)
	}
	if ar.Reason != "too risky" {
		t.Fatalf("expected the denial reason to be recorded, got %q", ar.Reason)
	}

	if err := e.Consume(ctx, d.ID, d.ActionDigest); err != ErrNotAdmitted {
		t.Fatalf("expected the denied decision to remain non-consumable, got %v", err)
	}
}

func TestDeny_AlreadyResolved_Fails(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()

	d, err := e.Decide(ctx, DecideRequest{OperationID: "op-1", Payload: map[string]any{"x": 1}, Reversibility: ReversibilityNone})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, err := e.Deny(ctx, d.ApprovalRequestID, "operator:jane", "no"); err != nil {
		t.Fatalf("first Deny: %v", err)
	}
	if _, err := e.Deny(ctx, d.ApprovalRequestID, "operator:jane", "no"); err != ErrNotPending {
		t.Fatalf("expected ErrNotPending on a second Deny, got %v", err)
	}
}

func TestListPendingApprovals_OnlyPending(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()

	d1, _ := e.Decide(ctx, DecideRequest{OperationID: "op-1", Payload: map[string]any{"x": 1}, Reversibility: ReversibilityNone})
	d2, _ := e.Decide(ctx, DecideRequest{OperationID: "op-2", Payload: map[string]any{"x": 2}, Reversibility: ReversibilityNone})
	if _, err := e.Approve(ctx, d1.ApprovalRequestID, "operator:jane"); err != nil {
		t.Fatalf("Approve d1: %v", err)
	}

	pending, err := e.ListPendingApprovals(ctx)
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	if len(pending) != 1 || pending[0].DecisionID != d2.ID {
		t.Fatalf("expected exactly the still-pending d2 approval, got %+v", pending)
	}
}

func TestGetApprovalRequest_UnknownID_ReturnsNotFound(t *testing.T) {
	e := New(testDB(t))
	if _, err := e.GetApprovalRequest(context.Background(), "does-not-exist"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
