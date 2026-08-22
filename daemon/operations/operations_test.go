package operations

import (
	"context"
	"errors"
	"testing"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/policy"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store/storetest"
)

func testEngine(t *testing.T) *Engine {
	t.Helper()
	db := storetest.Open(t, "../../store/migrations")
	return New(db, policy.New(db))
}

func proposeVerified(t *testing.T, e *Engine, operationID string) *Effect {
	t.Helper()
	eff, err := e.Propose(context.Background(), ProposeRequest{
		OperationID:      operationID,
		OwnerExtensionID: "amh.test/widget",
		EffectType:       "amh.test/do-thing",
		Payload:          map[string]any{"op": operationID},
		Reversibility:    policy.ReversibilityVerified,
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	return eff
}

func TestPropose_VerifiedReversibility_Admits(t *testing.T) {
	e := testEngine(t)
	eff := proposeVerified(t, e, "op-1")
	if eff.State != StateAdmitted {
		t.Fatalf("expected admitted, got %s", eff.State)
	}
	if eff.DecisionID == "" || eff.ForwardDigest == "" {
		t.Fatalf("expected a bound decision and digest, got %+v", eff)
	}
}

func TestPropose_UnverifiedReversibility_NeedsApproval(t *testing.T) {
	e := testEngine(t)
	eff, err := e.Propose(context.Background(), ProposeRequest{
		OperationID:      "op-1",
		OwnerExtensionID: "amh.test/widget",
		EffectType:       "amh.test/do-thing",
		Payload:          map[string]any{"op": "op-1"},
		Reversibility:    policy.ReversibilityNone,
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if eff.State != StateNeedsApproval {
		t.Fatalf("expected needs_approval, got %s", eff.State)
	}
}

func TestFullHappyPath_ProposeDispatchObserveConfirm(t *testing.T) {
	e := testEngine(t)
	eff := proposeVerified(t, e, "op-1")

	eff, err := e.MarkDispatchPending(context.Background(), eff.EffectID)
	if err != nil {
		t.Fatalf("MarkDispatchPending: %v", err)
	}
	if eff.State != StateDispatchPending {
		t.Fatalf("expected dispatch_pending, got %s", eff.State)
	}

	eff, err = e.MarkDispatched(context.Background(), eff.EffectID, "cmd-123")
	if err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}
	if eff.State != StateDispatched || eff.ExternalCommandID != "cmd-123" {
		t.Fatalf("expected dispatched with external_command_id, got %+v", eff)
	}

	eff, err = e.MarkObserved(context.Background(), eff.EffectID, "artifact://obs-1")
	if err != nil {
		t.Fatalf("MarkObserved: %v", err)
	}
	if eff.State != StateObserved {
		t.Fatalf("expected observed, got %s", eff.State)
	}

	eff, err = e.Resolve(context.Background(), eff.EffectID, StateConfirmed, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if eff.State != StateConfirmed {
		t.Fatalf("expected confirmed, got %s", eff.State)
	}

	// The admitting decision must be consumed exactly once — a second
	// MarkDispatchPending-style consume attempt would now fail.
	if err := e.Policy.Consume(context.Background(), eff.DecisionID, eff.ForwardDigest); !errors.Is(err, policy.ErrAlreadyConsumed) {
		t.Fatalf("expected the admitting decision to already be consumed, got %v", err)
	}
}

func TestMarkDispatchPending_RequiresAdmitted(t *testing.T) {
	e := testEngine(t)
	eff, err := e.Propose(context.Background(), ProposeRequest{
		OperationID:      "op-1",
		OwnerExtensionID: "amh.test/widget",
		EffectType:       "amh.test/do-thing",
		Payload:          map[string]any{},
		Reversibility:    policy.ReversibilityNone,
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if _, err := e.MarkDispatchPending(context.Background(), eff.EffectID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for a needs_approval effect, got %v", err)
	}
}

func TestMarkObserved_RequiresDispatched(t *testing.T) {
	e := testEngine(t)
	eff := proposeVerified(t, e, "op-1")
	if _, err := e.MarkObserved(context.Background(), eff.EffectID, "ref"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for a still-admitted effect, got %v", err)
	}
}

func TestResolve_RequiresObservedOrOutcomeUnknown(t *testing.T) {
	e := testEngine(t)
	eff := proposeVerified(t, e, "op-1")
	if _, err := e.Resolve(context.Background(), eff.EffectID, StateConfirmed, nil); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for a still-admitted effect, got %v", err)
	}
}

func TestResolve_RejectsNonTerminalState(t *testing.T) {
	e := testEngine(t)
	eff := proposeVerified(t, e, "op-1")
	if _, err := e.Resolve(context.Background(), eff.EffectID, StateDispatched, nil); err == nil {
		t.Fatalf("expected an error resolving to a non-terminal state")
	}
}

func TestReconcileInterrupted_MarksStuckDispatchedEffects(t *testing.T) {
	e := testEngine(t)

	stuck := proposeVerified(t, e, "op-stuck")
	stuck, err := e.MarkDispatchPending(context.Background(), stuck.EffectID)
	if err != nil {
		t.Fatalf("MarkDispatchPending: %v", err)
	}
	stuck, err = e.MarkDispatched(context.Background(), stuck.EffectID, "cmd-stuck")
	if err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}
	// Simulate the daemon crashing right here: nothing ever observes
	// this effect's outcome.

	notStuck := proposeVerified(t, e, "op-fine")

	reconciled, err := e.ReconcileInterrupted(context.Background())
	if err != nil {
		t.Fatalf("ReconcileInterrupted: %v", err)
	}
	if len(reconciled) != 1 || reconciled[0].EffectID != stuck.EffectID {
		t.Fatalf("expected exactly the stuck effect to be reconciled, got %+v", reconciled)
	}

	got, err := e.Get(context.Background(), stuck.EffectID)
	if err != nil {
		t.Fatalf("Get stuck: %v", err)
	}
	if got.State != StateOutcomeUnknown {
		t.Fatalf("expected outcome_unknown, got %s", got.State)
	}

	untouched, err := e.Get(context.Background(), notStuck.EffectID)
	if err != nil {
		t.Fatalf("Get not-stuck: %v", err)
	}
	if untouched.State != StateAdmitted {
		t.Fatalf("expected the non-dispatched effect to be untouched, got %s", untouched.State)
	}
}

func TestReconcileInterrupted_ThenResolve_ReachesReconciled(t *testing.T) {
	e := testEngine(t)
	eff := proposeVerified(t, e, "op-1")
	eff, err := e.MarkDispatchPending(context.Background(), eff.EffectID)
	if err != nil {
		t.Fatalf("MarkDispatchPending: %v", err)
	}
	eff, err = e.MarkDispatched(context.Background(), eff.EffectID, "cmd-1")
	if err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}

	if _, err := e.ReconcileInterrupted(context.Background()); err != nil {
		t.Fatalf("ReconcileInterrupted: %v", err)
	}

	eff, err = e.Resolve(context.Background(), eff.EffectID, StateReconciled, &EffectError{
		Code: "TIMEOUT_THEN_CONFIRMED", Retryable: false, Message: "extension confirmed the effect completed despite the timeout",
	})
	if err != nil {
		t.Fatalf("Resolve after reconciliation: %v", err)
	}
	if eff.State != StateReconciled {
		t.Fatalf("expected reconciled, got %s", eff.State)
	}
	if eff.ErrorCode != "TIMEOUT_THEN_CONFIRMED" {
		t.Fatalf("expected error detail to be recorded, got %+v", eff)
	}
}

func TestGet_UnknownEffect(t *testing.T) {
	e := testEngine(t)
	if _, err := e.Get(context.Background(), "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestListByOperation(t *testing.T) {
	e := testEngine(t)
	proposeVerified(t, e, "op-1")

	list, err := e.ListByOperation(context.Background(), "op-1")
	if err != nil {
		t.Fatalf("ListByOperation: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly one effect for op-1, got %d", len(list))
	}
}
