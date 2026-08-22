package selfimprove

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store/storetest"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return storetest.Open(t, "../../store/migrations")
}

func passResults(n int) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = true
	}
	return out
}

func failResults(n int) []bool {
	return make([]bool, n) // all false
}

func TestGenerate_InvalidClass_Fails(t *testing.T) {
	e := New(testDB(t))
	if _, err := e.Generate(context.Background(), "not-a-real-class", "ref-1", "optimizer-x"); err != ErrInvalidClass {
		t.Fatalf("expected ErrInvalidClass, got %v", err)
	}
}

func TestGenerate_Valid_StartsGenerated(t *testing.T) {
	e := New(testDB(t))
	c, err := e.Generate(context.Background(), ClassPrompt, "prompt-digest-1", "optimizer-x")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if c.Status != StatusGenerated {
		t.Fatalf("expected generated, got %s", c.Status)
	}
}

func TestRecordEval_PassingRateAdmits_GeneratedToEvaluated(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()
	c, err := e.Generate(ctx, ClassPrompt, "ref-1", "optimizer-x")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	ev, err := e.RecordEval(ctx, c.ID, "eval-suite", "1.0.0", passResults(10))
	if err != nil {
		t.Fatalf("RecordEval: %v", err)
	}
	if !ev.Passed {
		t.Fatalf("expected a 100%% pass rate to pass, got %+v", ev)
	}

	got, err := e.Get(ctx, c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusEvaluated {
		t.Fatalf("expected evaluated, got %s", got.Status)
	}
}

func TestRecordEval_BelowThreshold_RejectsGenerated(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()
	c, err := e.Generate(ctx, ClassPrompt, "ref-1", "optimizer-x")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// 5/10 = 50% < MinPassRate (90%).
	results := passResults(10)
	for i := 0; i < 5; i++ {
		results[i] = false
	}
	ev, err := e.RecordEval(ctx, c.ID, "eval-suite", "1.0.0", results)
	if err != nil {
		t.Fatalf("RecordEval: %v", err)
	}
	if ev.Passed {
		t.Fatalf("expected a 50%% pass rate to fail, got %+v", ev)
	}

	got, err := e.Get(ctx, c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusRejected {
		t.Fatalf("expected rejected, got %s", got.Status)
	}
}

func TestRecordEval_CallerCannotDeclareVerdict(t *testing.T) {
	// The whole point: RecordEval takes raw case results, not a "passed"
	// bool — this test exists to document and pin that API shape. A
	// failing case set must produce a failing verdict no matter what.
	e := New(testDB(t))
	ctx := context.Background()
	c, err := e.Generate(ctx, ClassPrompt, "ref-1", "optimizer-x")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	ev, err := e.RecordEval(ctx, c.ID, "eval-suite", "1.0.0", failResults(10))
	if err != nil {
		t.Fatalf("RecordEval: %v", err)
	}
	if ev.Passed {
		t.Fatalf("expected an all-failing case set to never pass, got %+v", ev)
	}
}

func promoteThroughCanary(t *testing.T, e *Engine, ctx context.Context, class CandidateClass, ref string) *CandidateVersion {
	t.Helper()
	c, err := e.Generate(ctx, class, ref, "optimizer-x")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := e.RecordEval(ctx, c.ID, "eval-suite", "1.0.0", passResults(10)); err != nil {
		t.Fatalf("RecordEval (pre-canary): %v", err)
	}
	if _, err := e.Canary(ctx, c.ID); err != nil {
		t.Fatalf("Canary: %v", err)
	}
	if _, err := e.RecordEval(ctx, c.ID, "eval-suite", "1.0.0", passResults(10)); err != nil {
		t.Fatalf("RecordEval (canary): %v", err)
	}
	promoted, err := e.Promote(ctx, c.ID)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	return promoted
}

func TestFullLifecycle_GenerateEvalCanaryPromote(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()
	promoted := promoteThroughCanary(t, e, ctx, ClassPrompt, "ref-1")
	if promoted.Status != StatusPromoted {
		t.Fatalf("expected promoted, got %s", promoted.Status)
	}
	if promoted.RollbackTargetID != "" {
		t.Fatalf("expected no rollback target for the first-ever promotion of this class, got %q", promoted.RollbackTargetID)
	}
}

func TestPromote_WithoutCanaryStage_Fails(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()
	c, err := e.Generate(ctx, ClassPrompt, "ref-1", "optimizer-x")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := e.RecordEval(ctx, c.ID, "eval-suite", "1.0.0", passResults(10)); err != nil {
		t.Fatalf("RecordEval: %v", err)
	}
	if _, err := e.Promote(ctx, c.ID); err != ErrInvalidTransition {
		t.Fatalf("expected ErrInvalidTransition promoting straight from evaluated, got %v", err)
	}
}

func TestPromote_NoEvidenceSinceCanaryStarted_Fails(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()
	c, err := e.Generate(ctx, ClassPrompt, "ref-1", "optimizer-x")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// Only a pre-canary passing eval — none recorded after Canary().
	if _, err := e.RecordEval(ctx, c.ID, "eval-suite", "1.0.0", passResults(10)); err != nil {
		t.Fatalf("RecordEval: %v", err)
	}
	if _, err := e.Canary(ctx, c.ID); err != nil {
		t.Fatalf("Canary: %v", err)
	}
	if _, err := e.Promote(ctx, c.ID); err != ErrNoCanaryEvidence {
		t.Fatalf("expected ErrNoCanaryEvidence, got %v", err)
	}
}

func TestRecordEval_FailDuringCanary_Rejects(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()
	c, err := e.Generate(ctx, ClassPrompt, "ref-1", "optimizer-x")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := e.RecordEval(ctx, c.ID, "eval-suite", "1.0.0", passResults(10)); err != nil {
		t.Fatalf("RecordEval: %v", err)
	}
	if _, err := e.Canary(ctx, c.ID); err != nil {
		t.Fatalf("Canary: %v", err)
	}
	if _, err := e.RecordEval(ctx, c.ID, "eval-suite", "1.0.0", failResults(10)); err != nil {
		t.Fatalf("RecordEval (failing canary): %v", err)
	}
	got, err := e.Get(ctx, c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusRejected {
		t.Fatalf("expected a canary regression to reject the candidate, got %s", got.Status)
	}
}

func TestPromote_DemotesPreviousAndSetsRollbackTarget(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()

	first := promoteThroughCanary(t, e, ctx, ClassPrompt, "ref-1")
	second := promoteThroughCanary(t, e, ctx, ClassPrompt, "ref-2")

	if second.RollbackTargetID != first.ID {
		t.Fatalf("expected the second promotion's rollback target to be the first, got %q want %q", second.RollbackTargetID, first.ID)
	}

	gotFirst, err := e.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	if gotFirst.Status != StatusDemoted {
		t.Fatalf("expected the first candidate demoted, got %s", gotFirst.Status)
	}
}

func TestDemoteThenRollback_RestoresPriorBinding(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()

	first := promoteThroughCanary(t, e, ctx, ClassPrompt, "ref-1")
	second := promoteThroughCanary(t, e, ctx, ClassPrompt, "ref-2")

	demoted, err := e.Demote(ctx, second.ID)
	if err != nil {
		t.Fatalf("Demote: %v", err)
	}
	if demoted.Status != StatusDemoted {
		t.Fatalf("expected demoted, got %s", demoted.Status)
	}

	rolledBack, err := e.Rollback(ctx, second.ID)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rolledBack.Status != StatusRolledBack {
		t.Fatalf("expected rolled_back, got %s", rolledBack.Status)
	}

	restored, err := e.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	if restored.Status != StatusPromoted {
		t.Fatalf("expected the prior candidate restored to promoted, got %s", restored.Status)
	}
	if restored.RollbackTargetID != second.ID {
		t.Fatalf("expected the restored candidate's rollback target to point back at the rolled-back one, got %q", restored.RollbackTargetID)
	}
}

func TestRollback_WithoutRollbackTarget_Fails(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()
	first := promoteThroughCanary(t, e, ctx, ClassPrompt, "ref-1")
	if _, err := e.Demote(ctx, first.ID); err != nil {
		t.Fatalf("Demote: %v", err)
	}
	if _, err := e.Rollback(ctx, first.ID); err != ErrNoRollbackTarget {
		t.Fatalf("expected ErrNoRollbackTarget for the first-ever promotion of a class, got %v", err)
	}
}

func TestRollback_NotDemoted_Fails(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()
	c := promoteThroughCanary(t, e, ctx, ClassPrompt, "ref-1")
	if _, err := e.Rollback(ctx, c.ID); err != ErrInvalidTransition {
		t.Fatalf("expected ErrInvalidTransition rolling back a still-promoted candidate, got %v", err)
	}
}

func TestReject_ManualOverride(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()
	c, err := e.Generate(ctx, ClassPrompt, "ref-1", "optimizer-x")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	rejected, err := e.Reject(ctx, c.ID)
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if rejected.Status != StatusRejected {
		t.Fatalf("expected rejected, got %s", rejected.Status)
	}
}

func TestReject_AlreadyTerminal_Fails(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()
	c := promoteThroughCanary(t, e, ctx, ClassPrompt, "ref-1")
	if _, err := e.Reject(ctx, c.ID); err != ErrInvalidTransition {
		t.Fatalf("expected ErrInvalidTransition rejecting an already-promoted candidate, got %v", err)
	}
}

func TestGetAndCanaryAndDemote_UnknownID_ReturnsNotFound(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()
	if _, err := e.Get(ctx, "does-not-exist"); err != ErrNotFound {
		t.Fatalf("Get: expected ErrNotFound, got %v", err)
	}
	if _, err := e.Canary(ctx, "does-not-exist"); err != ErrNotFound {
		t.Fatalf("Canary: expected ErrNotFound, got %v", err)
	}
	if _, err := e.Demote(ctx, "does-not-exist"); err != ErrNotFound {
		t.Fatalf("Demote: expected ErrNotFound, got %v", err)
	}
}

func TestList_FiltersByClassAndStatus(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()

	if _, err := e.Generate(ctx, ClassPrompt, "ref-1", "optimizer-x"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := e.Generate(ctx, ClassSkill, "ref-2", "optimizer-x"); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	prompts, err := e.List(ctx, ListFilter{Class: ClassPrompt})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(prompts) != 1 || prompts[0].CandidateClass != ClassPrompt {
		t.Fatalf("expected exactly one prompt candidate, got %+v", prompts)
	}

	generated, err := e.List(ctx, ListFilter{Status: StatusGenerated})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(generated) != 2 {
		t.Fatalf("expected both candidates in generated status, got %+v", generated)
	}
}

// TestPromote_ConcurrentPromotionsOfSameClass_Serializes proves the
// class-keyed advisory lock (and the partial unique index as a backstop)
// actually prevent two candidates of the same class from ending up
// simultaneously 'promoted' when two Promote calls race — the exact
// failure mode a per-row lock alone cannot prevent, since each call locks
// a different candidate row.
func TestPromote_ConcurrentPromotionsOfSameClass_Serializes(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()

	// Both candidates reach 'canary' with real evidence before either
	// Promote call starts — only the promotion itself races.
	readyToPromote := func(ref string) *CandidateVersion {
		c, err := e.Generate(ctx, ClassPrompt, ref, "optimizer-x")
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if _, err := e.RecordEval(ctx, c.ID, "eval-suite", "1.0.0", passResults(10)); err != nil {
			t.Fatalf("RecordEval: %v", err)
		}
		if _, err := e.Canary(ctx, c.ID); err != nil {
			t.Fatalf("Canary: %v", err)
		}
		if _, err := e.RecordEval(ctx, c.ID, "eval-suite", "1.0.0", passResults(10)); err != nil {
			t.Fatalf("RecordEval (canary): %v", err)
		}
		return c
	}
	a := readyToPromote("ref-a")
	b := readyToPromote("ref-b")

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, errs[0] = e.Promote(ctx, a.ID)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, errs[1] = e.Promote(ctx, b.ID)
	}()
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Promote %d: %v", i, err)
		}
	}

	promoted, err := e.List(ctx, ListFilter{Class: ClassPrompt, Status: StatusPromoted})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(promoted) != 1 {
		t.Fatalf("expected exactly one promoted prompt candidate after the race, got %+v", promoted)
	}
}

func TestPromote_PassingEvalInSameMillisecondAsCanaryStart_StillCounts(t *testing.T) {
	e := New(testDB(t))
	ctx := context.Background()
	c, err := e.Generate(ctx, ClassPrompt, "ref-1", "optimizer-x")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := e.RecordEval(ctx, c.ID, "eval-suite", "1.0.0", passResults(10)); err != nil {
		t.Fatalf("RecordEval: %v", err)
	}
	if _, err := e.Canary(ctx, c.ID); err != nil {
		t.Fatalf("Canary: %v", err)
	}

	// Force the canary eval's evaluated_at to be textually EQUAL to
	// canary_at (the same-millisecond collision a strict `>` would
	// wrongly reject) rather than relying on timing to land there.
	ev, err := e.RecordEval(ctx, c.ID, "eval-suite", "1.0.0", passResults(10))
	if err != nil {
		t.Fatalf("RecordEval (canary): %v", err)
	}
	got, err := e.Get(ctx, c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := e.DB.ExecContext(ctx, `UPDATE eval SET evaluated_at = $1 WHERE id = $2`, got.CanaryAt, ev.ID); err != nil {
		t.Fatalf("force same-millisecond eval: %v", err)
	}

	if _, err := e.Promote(ctx, c.ID); err != nil {
		t.Fatalf("expected Promote to accept same-millisecond canary evidence, got %v", err)
	}
}
