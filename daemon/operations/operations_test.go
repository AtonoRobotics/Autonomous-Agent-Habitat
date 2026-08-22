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

// dispatchPayloadFor is the exact payload proposeVerified admits under
// operationID — MarkDispatchPending now hashes whatever payload it's
// given fresh (§15 invariant #6), so a test that means to walk the real
// happy path must pass back the same payload that was admitted.
func dispatchPayloadFor(operationID string) any {
	return map[string]any{"op": operationID}
}

func proposeVerified(t *testing.T, e *Engine, operationID string) *Effect {
	t.Helper()
	eff, err := e.Propose(context.Background(), ProposeRequest{
		OperationID:      operationID,
		OwnerExtensionID: "amh.test/widget",
		EffectType:       "amh.test/do-thing",
		Payload:          dispatchPayloadFor(operationID),
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

	eff, err := e.MarkDispatchPending(context.Background(), eff.EffectID, dispatchPayloadFor("op-1"))
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
	if _, err := e.MarkDispatchPending(context.Background(), eff.EffectID, map[string]any{}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for a needs_approval effect, got %v", err)
	}
}

// TestMarkDispatchPending_PayloadMutatedAfterAdmission_FailsClosed is the
// direct proof of §15 acceptance invariant #6 ("policy dispatch is bound
// to the admitted action digest ... after ... mutation"): an effect
// admitted for one payload must not be dispatchable under a different
// one, and the failure must be the real digest-mismatch check — not a
// pass-through that always agrees with itself.
func TestMarkDispatchPending_PayloadMutatedAfterAdmission_FailsClosed(t *testing.T) {
	e := testEngine(t)
	eff := proposeVerified(t, e, "op-1")

	mutated := map[string]any{"op": "op-1", "amount": 1_000_000}
	if _, err := e.MarkDispatchPending(context.Background(), eff.EffectID, mutated); !errors.Is(err, policy.ErrDigestMismatch) {
		t.Fatalf("expected ErrDigestMismatch for a payload that differs from the admitted one, got %v", err)
	}

	// Fails closed, not merely "returns an error": the effect must stay
	// admitted, not silently advance to dispatch_pending.
	got, err := e.Get(context.Background(), eff.EffectID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateAdmitted {
		t.Fatalf("expected the effect to remain admitted after a digest mismatch, got %s", got.State)
	}

	// The original, correct payload still dispatches — a mismatch on one
	// attempt must not have poisoned or consumed the decision.
	dispatched, err := e.MarkDispatchPending(context.Background(), eff.EffectID, dispatchPayloadFor("op-1"))
	if err != nil {
		t.Fatalf("MarkDispatchPending with the real payload: %v", err)
	}
	if dispatched.State != StateDispatchPending {
		t.Fatalf("expected dispatch_pending, got %s", dispatched.State)
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
	stuck, err := e.MarkDispatchPending(context.Background(), stuck.EffectID, dispatchPayloadFor("op-stuck"))
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
	eff, err := e.MarkDispatchPending(context.Background(), eff.EffectID, dispatchPayloadFor("op-1"))
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
