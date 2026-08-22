package extensions

import (
	"context"
	"testing"
)

func TestRegisterContextEffect_RequiresActiveExtension(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()
	discoverForCapabilityTest(t, reg, "amh.test/widget", "1.0.0")

	if _, err := reg.RegisterContextEffect(ctx, "amh.test/widget", "1.0.0", "tool", "search"); err == nil {
		t.Fatalf("expected registering a context effect on a not-yet-active extension to be refused")
	}
}

func TestRegisterContextEffect_AssignsMonotonicSequence(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()
	discoverForCapabilityTest(t, reg, "amh.test/widget", "1.0.0")
	if _, err := reg.Activate(ctx, "amh.test/widget", "1.0.0"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	first, err := reg.RegisterContextEffect(ctx, "amh.test/widget", "1.0.0", "tool", "search")
	if err != nil {
		t.Fatalf("register first: %v", err)
	}
	second, err := reg.RegisterContextEffect(ctx, "amh.test/widget", "1.0.0", "route", "/webhook")
	if err != nil {
		t.Fatalf("register second: %v", err)
	}
	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("expected sequences 1, 2, got %d, %d", first.Sequence, second.Sequence)
	}
	if first.ID == second.ID {
		t.Fatalf("expected distinct effect ids")
	}
}

func TestDisposeContextEffect_RequiresReverseRegistrationOrder(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()
	discoverForCapabilityTest(t, reg, "amh.test/widget", "1.0.0")
	reg.Activate(ctx, "amh.test/widget", "1.0.0")

	first, err := reg.RegisterContextEffect(ctx, "amh.test/widget", "1.0.0", "tool", "search")
	if err != nil {
		t.Fatalf("register first: %v", err)
	}
	second, err := reg.RegisterContextEffect(ctx, "amh.test/widget", "1.0.0", "route", "/webhook")
	if err != nil {
		t.Fatalf("register second: %v", err)
	}

	// Disposing the first (oldest) while the second (newest) is still
	// outstanding violates reverse registration order.
	if err := reg.DisposeContextEffect(ctx, "amh.test/widget", "1.0.0", first.ID); err == nil {
		t.Fatalf("expected disposing out of reverse order to be refused")
	}

	// The real reverse order works: newest first, then oldest.
	if err := reg.DisposeContextEffect(ctx, "amh.test/widget", "1.0.0", second.ID); err != nil {
		t.Fatalf("dispose second (LIFO-correct): %v", err)
	}
	if err := reg.DisposeContextEffect(ctx, "amh.test/widget", "1.0.0", first.ID); err != nil {
		t.Fatalf("dispose first, now the only outstanding one: %v", err)
	}
}

func TestListOutstandingContextEffects_ExcludesDisposedOnes(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()
	discoverForCapabilityTest(t, reg, "amh.test/widget", "1.0.0")
	reg.Activate(ctx, "amh.test/widget", "1.0.0")

	first, _ := reg.RegisterContextEffect(ctx, "amh.test/widget", "1.0.0", "tool", "search")
	reg.RegisterContextEffect(ctx, "amh.test/widget", "1.0.0", "route", "/webhook")

	outstanding, err := reg.ListOutstandingContextEffects(ctx, "amh.test/widget", "1.0.0")
	if err != nil {
		t.Fatalf("ListOutstandingContextEffects: %v", err)
	}
	if len(outstanding) != 2 {
		t.Fatalf("expected 2 outstanding effects, got %d", len(outstanding))
	}

	second := outstanding[1]
	if err := reg.DisposeContextEffect(ctx, "amh.test/widget", "1.0.0", second.ID); err != nil {
		t.Fatalf("dispose second: %v", err)
	}

	outstanding, err = reg.ListOutstandingContextEffects(ctx, "amh.test/widget", "1.0.0")
	if err != nil {
		t.Fatalf("ListOutstandingContextEffects after dispose: %v", err)
	}
	if len(outstanding) != 1 || outstanding[0].ID != first.ID {
		t.Fatalf("expected only the first effect still outstanding, got %+v", outstanding)
	}
}

// TestDispose_RefusesWhileContextEffectsOutstanding is the direct proof
// of §15 acceptance invariant #3 / §5.1: an extension that registered a
// core-mediated effect and never disposed it cannot be disposed itself —
// the same fail-closed posture Quiesce already takes for active
// dependents (invariant #4).
func TestDispose_RefusesWhileContextEffectsOutstanding(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	m := baseManifest("amh.test/widget", "1.0.0")
	reg.Discover(ctx, m)
	reg.Activate(ctx, "amh.test/widget", "1.0.0")
	reg.RegisterContextEffect(ctx, "amh.test/widget", "1.0.0", "tool", "search")

	if _, err := reg.Quiesce(ctx, "amh.test/widget", "1.0.0"); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if _, err := reg.Dispose(ctx, "amh.test/widget", "1.0.0"); err == nil {
		t.Fatalf("expected Dispose to be refused while a core-mediated effect is still outstanding")
	}

	// Extension status must be unchanged by the refused Dispose — still
	// quiescing, not silently advanced or left ambiguous.
	ext, err := reg.Get(ctx, "amh.test/widget", "1.0.0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ext.Status != StatusQuiescing {
		t.Fatalf("expected quiescing after a refused Dispose, got %s", ext.Status)
	}
}

func TestDispose_SucceedsOnceAllContextEffectsAreDisposed(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	m := baseManifest("amh.test/widget", "1.0.0")
	reg.Discover(ctx, m)
	reg.Activate(ctx, "amh.test/widget", "1.0.0")
	eff, _ := reg.RegisterContextEffect(ctx, "amh.test/widget", "1.0.0", "tool", "search")

	reg.Quiesce(ctx, "amh.test/widget", "1.0.0")
	if _, err := reg.Dispose(ctx, "amh.test/widget", "1.0.0"); err == nil {
		t.Fatalf("expected Dispose to be refused first, while the effect is outstanding")
	}

	if err := reg.DisposeContextEffect(ctx, "amh.test/widget", "1.0.0", eff.ID); err != nil {
		t.Fatalf("DisposeContextEffect: %v", err)
	}

	disposed, err := reg.Dispose(ctx, "amh.test/widget", "1.0.0")
	if err != nil {
		t.Fatalf("Dispose after all context effects disposed: %v", err)
	}
	if disposed.Status != StatusDisposed {
		t.Fatalf("expected disposed, got %s", disposed.Status)
	}
}
