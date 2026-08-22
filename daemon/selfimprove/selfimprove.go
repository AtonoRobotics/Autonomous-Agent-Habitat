// Package selfimprove is AMH's self-improvement candidate lifecycle
// engine (docs/AMH-SPECIFICATION.md §10; §2.1: "independent evaluation,
// canary, promotion, demotion, and rollback mechanics"). It exists so
// decision 10 ("self-change is offline, evaluated, canaried, and
// reversible") is a mechanical property of the core, the same way
// daemon/policy makes decision 9 mechanical: GENERATED -> EVALUATED ->
// CANARY -> PROMOTED | REJECTED; PROMOTED -> DEMOTED -> ROLLED_BACK.
//
// § 10 is explicit that "no optimizer may alter its evaluator, held-out
// cases, instrumentation, policy decision, approval, or promotion
// threshold." This package enforces that two ways, not one:
// RecordEval takes raw, caller-supplied per-case pass/fail results and
// computes the passed verdict itself against a fixed threshold
// (MinPassRate) — a caller declares measurements, never the verdict,
// exactly the "core computes the verdict from a declared property"
// discipline daemon/policy already applies to a proposed action's
// reversibility. But a server-computed verdict over caller-supplied
// measurements is only as independent as the caller submitting them:
// daemon/api's route table (see its own doc comment) gates RecordEval
// at the operator tier, not the "agents propose" tier Generate sits at
// — an agent holding only its own token can propose a candidate, but
// cannot also be the one asserting it passed.
//
// # What this package does NOT do
//
// GEPA, ACE, Voyager-style skill extraction, DSPy optimizers, and coder
// agents are the candidate-GENERATING side (§10: "replaceable modules") —
// none of them exist in this codebase, so nothing currently calls
// Generate with a real, produced-by-an-optimizer candidate. And no
// existing prompt/skill/retrieval-policy call site (agents/workflows/
// goal.py's hardcoded prompts, for instance) reads from or rebinds to a
// promoted CandidateVersion — Promote is real, durable state, not yet a
// live capability switch. Both are real, separable follow-up work,
// exactly the same class of honest scope cut daemon/a2a's own doc
// comment already documents for autonomous goal dispatch: this package
// provides the mechanics §2.1 requires; it does not itself produce or
// consume candidates.
//
// "Canary" here means a stricter evidence bar (a passing Eval recorded
// after entering the canary stage, not merely a historical pass) rather
// than live traffic-splitting — no request-routing infrastructure to
// divert a fraction of real calls to a canary candidate exists either.
package selfimprove

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound          = errors.New("selfimprove: candidate not found")
	ErrInvalidClass      = errors.New("selfimprove: invalid candidate_class")
	ErrInvalidTransition = errors.New("selfimprove: candidate is not in a valid state for this transition")
	ErrNoCanaryEvidence  = errors.New("selfimprove: no passing eval recorded since entering canary")
	ErrNoRollbackTarget  = errors.New("selfimprove: candidate has no rollback_target_id")
	ErrRollbackBlocked   = errors.New("selfimprove: a different candidate of this class is currently promoted")
)

type CandidateClass string

const (
	ClassPrompt          CandidateClass = "prompt"
	ClassRetrievalPolicy CandidateClass = "retrieval_policy"
	ClassSkill           CandidateClass = "skill"
	ClassModule          CandidateClass = "module"
	ClassCoreCode        CandidateClass = "core_code"
)

func validClass(c CandidateClass) bool {
	switch c {
	case ClassPrompt, ClassRetrievalPolicy, ClassSkill, ClassModule, ClassCoreCode:
		return true
	default:
		return false
	}
}

type Status string

const (
	StatusGenerated  Status = "generated"
	StatusEvaluated  Status = "evaluated"
	StatusCanary     Status = "canary"
	StatusPromoted   Status = "promoted"
	StatusRejected   Status = "rejected"
	StatusDemoted    Status = "demoted"
	StatusRolledBack Status = "rolled_back"
)

// MinPassRate is the one fixed, core-owned promotion threshold every
// Eval is judged against — see the package doc comment on why this is a
// constant a caller cannot override, not a RecordEval parameter.
const MinPassRate = 0.9

// iso8601Format matches store/migrations' iso8601_now() output — see
// daemon/policy's identical constant and doc comment for why this
// matters.
const iso8601Format = "2006-01-02T15:04:05.000Z"

func iso8601(t time.Time) string { return t.UTC().Format(iso8601Format) }

// CandidateVersion is one row of candidate_version, as returned to
// callers.
type CandidateVersion struct {
	ID               string
	CandidateClass   CandidateClass
	Ref              string
	Status           Status
	GeneratedBy      string
	CreatedAt        string
	CanaryAt         string
	PromotedAt       string
	DemotedAt        string
	RolledBackAt     string
	RollbackTargetID string
}

// Eval is one row of eval, as returned to callers.
type Eval struct {
	ID                 string
	CandidateVersionID string
	EvaluatorID        string
	EvaluatorVersion   string
	Metrics            map[string]any
	Passed             bool
	EvaluatedAt        string
}

// Engine is the self-improvement lifecycle seam.
type Engine struct {
	DB *sql.DB
}

func New(db *sql.DB) *Engine {
	return &Engine{DB: db}
}

// Generate records a new candidate in the 'generated' state — the
// "agents propose" half of this package's own instance of decision 9.
func (e *Engine) Generate(ctx context.Context, class CandidateClass, ref, generatedBy string) (*CandidateVersion, error) {
	if !validClass(class) {
		return nil, ErrInvalidClass
	}
	if ref == "" {
		return nil, fmt.Errorf("selfimprove: ref is required")
	}
	id := uuid.NewString()
	if _, err := e.DB.ExecContext(ctx, `
		INSERT INTO candidate_version (id, candidate_class, ref, status, generated_by)
		VALUES ($1, $2, $3, 'generated', $4)`,
		id, string(class), ref, nullIfEmpty(generatedBy),
	); err != nil {
		return nil, fmt.Errorf("selfimprove: insert candidate_version: %w", err)
	}
	return e.Get(ctx, id)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// RecordEval computes passed from caseResults against the fixed
// MinPassRate threshold — see the package doc comment — and persists an
// immutable Eval row. A 'generated' candidate transitions to 'evaluated'
// on a pass or 'rejected' on a fail; a 'canary' candidate transitions to
// 'rejected' on a fail (canary evidence catching a regression) but stays
// 'canary' on a pass (promotion is always an explicit, separate Promote
// call, never automatic). An eval recorded against any other status
// (already promoted, already terminal) is still durably persisted, for
// ongoing monitoring evidence, but triggers no state transition.
func (e *Engine) RecordEval(ctx context.Context, candidateID, evaluatorID, evaluatorVersion string, caseResults []bool) (*Eval, error) {
	if len(caseResults) == 0 {
		return nil, fmt.Errorf("selfimprove: caseResults must be non-empty")
	}
	passCount := 0
	for _, ok := range caseResults {
		if ok {
			passCount++
		}
	}
	passRate := float64(passCount) / float64(len(caseResults))
	passed := passRate >= MinPassRate
	metrics := map[string]any{"pass_rate": passRate, "case_count": len(caseResults), "pass_count": passCount}
	metricsJSON, err := json.Marshal(metrics)
	if err != nil {
		return nil, fmt.Errorf("selfimprove: marshal metrics: %w", err)
	}

	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("selfimprove: begin eval tx: %w", err)
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM candidate_version WHERE id = $1 FOR UPDATE`, candidateID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("selfimprove: query candidate for eval: %w", err)
	}

	evalID := uuid.NewString()
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO eval (id, candidate_version_id, evaluator_id, evaluator_version, metrics, passed, evaluated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		evalID, candidateID, evaluatorID, evaluatorVersion, string(metricsJSON), passed, iso8601(now),
	); err != nil {
		return nil, fmt.Errorf("selfimprove: insert eval: %w", err)
	}

	var newStatus string
	switch Status(status) {
	case StatusGenerated:
		if passed {
			newStatus = string(StatusEvaluated)
		} else {
			newStatus = string(StatusRejected)
		}
	case StatusCanary:
		if !passed {
			newStatus = string(StatusRejected)
		}
	}
	if newStatus != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE candidate_version SET status = $1 WHERE id = $2`, newStatus, candidateID); err != nil {
			return nil, fmt.Errorf("selfimprove: transition candidate after eval: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("selfimprove: commit eval: %w", err)
	}
	return e.getEval(ctx, evalID)
}

// Canary moves an 'evaluated' candidate into the 'canary' stage,
// recording canary_at so Promote can later require evidence gathered
// after this point, not merely a historical pass.
func (e *Engine) Canary(ctx context.Context, candidateID string) (*CandidateVersion, error) {
	res, err := e.DB.ExecContext(ctx, `
		UPDATE candidate_version SET status = 'canary', canary_at = $1 WHERE id = $2 AND status = 'evaluated'`,
		iso8601(time.Now()), candidateID,
	)
	if err != nil {
		return nil, fmt.Errorf("selfimprove: canary: %w", err)
	}
	if err := requireOneRowAffected(ctx, e.DB, candidateID, res); err != nil {
		return nil, err
	}
	return e.Get(ctx, candidateID)
}

// Promote requires the candidate to be 'canary' with at least one
// passing Eval recorded after canary_at — real evidence gathered during
// the canary stage, not evidence carried over from before it. Any other
// candidate of the same class currently 'promoted' is demoted as part of
// the same transaction, and this candidate's rollback_target_id is set
// to it — "quiesce previous provider... switch binding, retain rollback
// target" (§10), at the bookkeeping level (see the package doc comment
// on why no live capability actually rebinds yet).
func (e *Engine) Promote(ctx context.Context, candidateID string) (*CandidateVersion, error) {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("selfimprove: begin promote tx: %w", err)
	}
	defer tx.Rollback()

	// Learn the class first (no row lock needed for this alone), so the
	// class-level advisory lock below can be acquired before anything
	// promotion-relevant is read. Locking only candidateID's own row (as
	// an earlier version of this function did) does NOT serialize two
	// concurrent Promote calls for two DIFFERENT candidates of the SAME
	// class: each transaction locks a different row, so both can read
	// "no previously promoted candidate" (or the same one) and both
	// commit as 'promoted', leaving two live bindings for one class.
	// pg_advisory_xact_lock blocks any other Promote/Rollback of this
	// class until this transaction ends (commit or rollback) — real
	// mutual exclusion a per-row lock cannot provide.
	var class string
	if err := tx.QueryRowContext(ctx, `SELECT candidate_class FROM candidate_version WHERE id = $1`, candidateID).Scan(&class); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("selfimprove: query candidate class for promote: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, class); err != nil {
		return nil, fmt.Errorf("selfimprove: acquire class promotion lock: %w", err)
	}

	var status string
	var canaryAt sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status, canary_at FROM candidate_version WHERE id = $1 FOR UPDATE`, candidateID).
		Scan(&status, &canaryAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("selfimprove: query candidate for promote: %w", err)
	}
	if Status(status) != StatusCanary {
		return nil, ErrInvalidTransition
	}

	// >= , not >: iso8601 truncates to millisecond precision, so a passing
	// eval recorded in the same millisecond Canary() set canary_at in is a
	// real, valid post-canary result that a strict > would wrongly reject.
	// This can only ever admit a truly pre-canary eval if that eval and
	// the Canary() call itself land in the same millisecond AND status
	// somehow already reads 'canary' before Canary()'s transaction
	// committed — impossible under Postgres MVCC, since RecordEval only
	// ever observes 'canary' after that transaction has actually committed.
	var passingEvalCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM eval WHERE candidate_version_id = $1 AND passed = true AND evaluated_at >= $2`,
		candidateID, canaryAt.String,
	).Scan(&passingEvalCount); err != nil {
		return nil, fmt.Errorf("selfimprove: query canary evidence: %w", err)
	}
	if passingEvalCount == 0 {
		return nil, ErrNoCanaryEvidence
	}

	var previousPromotedID sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM candidate_version WHERE candidate_class = $1 AND status = 'promoted' AND id != $2`,
		class, candidateID,
	).Scan(&previousPromotedID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("selfimprove: query previously promoted candidate: %w", err)
	}

	now := iso8601(time.Now())
	if previousPromotedID.Valid {
		if _, err := tx.ExecContext(ctx, `UPDATE candidate_version SET status = 'demoted', demoted_at = $1 WHERE id = $2`,
			now, previousPromotedID.String); err != nil {
			return nil, fmt.Errorf("selfimprove: demote previous candidate: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE candidate_version SET status = 'promoted', promoted_at = $1, rollback_target_id = $2 WHERE id = $3`,
		now, nullIfInvalid(previousPromotedID), candidateID,
	); err != nil {
		return nil, fmt.Errorf("selfimprove: promote candidate: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("selfimprove: commit promote: %w", err)
	}
	return e.Get(ctx, candidateID)
}

func nullIfInvalid(s sql.NullString) any {
	if !s.Valid {
		return nil
	}
	return s.String
}

// Demote moves a 'promoted' candidate to 'demoted' — the candidate stays
// eligible for Rollback afterward if it itself has a rollback_target_id
// (i.e. it was promoted over a prior candidate).
func (e *Engine) Demote(ctx context.Context, candidateID string) (*CandidateVersion, error) {
	res, err := e.DB.ExecContext(ctx, `
		UPDATE candidate_version SET status = 'demoted', demoted_at = $1 WHERE id = $2 AND status = 'promoted'`,
		iso8601(time.Now()), candidateID,
	)
	if err != nil {
		return nil, fmt.Errorf("selfimprove: demote: %w", err)
	}
	if err := requireOneRowAffected(ctx, e.DB, candidateID, res); err != nil {
		return nil, err
	}
	return e.Get(ctx, candidateID)
}

// Rollback requires candidateID to be 'demoted' with a rollback_target_id
// set, and restores that target to 'promoted' (acceptance invariant #11:
// "rollback restores the prior capability binding"). Fails closed
// (ErrRollbackBlocked) if some OTHER candidate of the same class has
// since been promoted — restoring the target would otherwise leave two
// simultaneously 'promoted' candidates of the same class, which nothing
// in this schema treats as a coherent state.
func (e *Engine) Rollback(ctx context.Context, candidateID string) (*CandidateVersion, error) {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("selfimprove: begin rollback tx: %w", err)
	}
	defer tx.Rollback()

	// Same class-level serialization as Promote, and for the same reason:
	// Rollback also mutates "at most one promoted candidate per class,"
	// so it must not race a concurrent Promote (or another Rollback) of
	// the same class. See Promote's doc comment.
	var class string
	if err := tx.QueryRowContext(ctx, `SELECT candidate_class FROM candidate_version WHERE id = $1`, candidateID).Scan(&class); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("selfimprove: query candidate class for rollback: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, class); err != nil {
		return nil, fmt.Errorf("selfimprove: acquire class promotion lock: %w", err)
	}

	var status string
	var rollbackTargetID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status, rollback_target_id FROM candidate_version WHERE id = $1 FOR UPDATE`, candidateID).
		Scan(&status, &rollbackTargetID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("selfimprove: query candidate for rollback: %w", err)
	}
	if Status(status) != StatusDemoted {
		return nil, ErrInvalidTransition
	}
	if !rollbackTargetID.Valid {
		return nil, ErrNoRollbackTarget
	}

	var currentlyPromotedCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM candidate_version WHERE candidate_class = $1 AND status = 'promoted'`, class,
	).Scan(&currentlyPromotedCount); err != nil {
		return nil, fmt.Errorf("selfimprove: query currently promoted: %w", err)
	}
	if currentlyPromotedCount > 0 {
		return nil, ErrRollbackBlocked
	}

	now := iso8601(time.Now())
	if _, err := tx.ExecContext(ctx, `UPDATE candidate_version SET status = 'rolled_back', rolled_back_at = $1 WHERE id = $2`,
		now, candidateID); err != nil {
		return nil, fmt.Errorf("selfimprove: mark rolled back: %w", err)
	}
	// Symmetric restore: the target becomes rollback-able again, back to
	// the candidate that was just rolled back, so a further rollback can
	// reverse this one too.
	if _, err := tx.ExecContext(ctx, `
		UPDATE candidate_version SET status = 'promoted', promoted_at = $1, rollback_target_id = $2 WHERE id = $3`,
		now, candidateID, rollbackTargetID.String,
	); err != nil {
		return nil, fmt.Errorf("selfimprove: restore rollback target: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("selfimprove: commit rollback: %w", err)
	}
	return e.Get(ctx, candidateID)
}

// Reject manually rejects a non-terminal candidate — the operator
// override alongside RecordEval's automatic rejection-on-fail path.
func (e *Engine) Reject(ctx context.Context, candidateID string) (*CandidateVersion, error) {
	res, err := e.DB.ExecContext(ctx, `
		UPDATE candidate_version SET status = 'rejected' WHERE id = $1 AND status IN ('generated', 'evaluated', 'canary')`,
		candidateID,
	)
	if err != nil {
		return nil, fmt.Errorf("selfimprove: reject: %w", err)
	}
	if err := requireOneRowAffected(ctx, e.DB, candidateID, res); err != nil {
		return nil, err
	}
	return e.Get(ctx, candidateID)
}

// requireOneRowAffected distinguishes ErrNotFound (no such candidate)
// from ErrInvalidTransition (candidate exists but wasn't in the required
// starting state) after a conditional UPDATE affected zero rows.
func requireOneRowAffected(ctx context.Context, db *sql.DB, candidateID string, res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("selfimprove: rows affected: %w", err)
	}
	if n > 0 {
		return nil
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM candidate_version WHERE id = $1)`, candidateID).Scan(&exists); err != nil {
		return fmt.Errorf("selfimprove: check candidate existence: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return ErrInvalidTransition
}

// Get fetches a candidate by id.
func (e *Engine) Get(ctx context.Context, id string) (*CandidateVersion, error) {
	var c CandidateVersion
	var generatedBy, canaryAt, promotedAt, demotedAt, rolledBackAt, rollbackTargetID sql.NullString
	err := e.DB.QueryRowContext(ctx, `
		SELECT id, candidate_class, ref, status, generated_by, created_at, canary_at, promoted_at, demoted_at, rolled_back_at, rollback_target_id
		FROM candidate_version WHERE id = $1`, id,
	).Scan(&c.ID, &c.CandidateClass, &c.Ref, &c.Status, &generatedBy, &c.CreatedAt, &canaryAt, &promotedAt, &demotedAt, &rolledBackAt, &rollbackTargetID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("selfimprove: get candidate: %w", err)
	}
	c.GeneratedBy = generatedBy.String
	c.CanaryAt = canaryAt.String
	c.PromotedAt = promotedAt.String
	c.DemotedAt = demotedAt.String
	c.RolledBackAt = rolledBackAt.String
	c.RollbackTargetID = rollbackTargetID.String
	return &c, nil
}

func (e *Engine) getEval(ctx context.Context, id string) (*Eval, error) {
	var ev Eval
	var metricsJSON string
	err := e.DB.QueryRowContext(ctx, `
		SELECT id, candidate_version_id, evaluator_id, evaluator_version, metrics, passed, evaluated_at
		FROM eval WHERE id = $1`, id,
	).Scan(&ev.ID, &ev.CandidateVersionID, &ev.EvaluatorID, &ev.EvaluatorVersion, &metricsJSON, &ev.Passed, &ev.EvaluatedAt)
	if err != nil {
		return nil, fmt.Errorf("selfimprove: get eval: %w", err)
	}
	if metricsJSON != "" {
		if err := json.Unmarshal([]byte(metricsJSON), &ev.Metrics); err != nil {
			return nil, fmt.Errorf("selfimprove: unmarshal metrics: %w", err)
		}
	}
	return &ev, nil
}

// ListFilter narrows List — every field is optional.
type ListFilter struct {
	Class  CandidateClass
	Status Status
}

// List returns candidates matching f, newest first.
func (e *Engine) List(ctx context.Context, f ListFilter) ([]CandidateVersion, error) {
	query := `SELECT id, candidate_class, ref, status, generated_by, created_at, canary_at, promoted_at, demoted_at, rolled_back_at, rollback_target_id FROM candidate_version WHERE 1=1`
	var args []any
	if f.Class != "" {
		args = append(args, string(f.Class))
		query += fmt.Sprintf(" AND candidate_class = $%d", len(args))
	}
	if f.Status != "" {
		args = append(args, string(f.Status))
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	query += " ORDER BY created_at DESC"

	rows, err := e.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("selfimprove: list candidates: %w", err)
	}
	defer rows.Close()

	out := []CandidateVersion{}
	for rows.Next() {
		var c CandidateVersion
		var generatedBy, canaryAt, promotedAt, demotedAt, rolledBackAt, rollbackTargetID sql.NullString
		if err := rows.Scan(&c.ID, &c.CandidateClass, &c.Ref, &c.Status, &generatedBy, &c.CreatedAt, &canaryAt, &promotedAt, &demotedAt, &rolledBackAt, &rollbackTargetID); err != nil {
			return nil, fmt.Errorf("selfimprove: scan candidate: %w", err)
		}
		c.GeneratedBy = generatedBy.String
		c.CanaryAt = canaryAt.String
		c.PromotedAt = promotedAt.String
		c.DemotedAt = demotedAt.String
		c.RolledBackAt = rolledBackAt.String
		c.RollbackTargetID = rollbackTargetID.String
		out = append(out, c)
	}
	return out, rows.Err()
}
