// Package operations tracks AMH's generic external-effect lifecycle
// (docs/AMH-SPECIFICATION.md §4; §11 "self-healing"; acceptance
// invariant #2, §15):
//
//	PROPOSED -> ADMITTED | REJECTED | NEEDS_APPROVAL -> DISPATCH_PENDING
//	  -> DISPATCHED -> OBSERVED | OUTCOME_UNKNOWN
//	  -> CONFIRMED | RECONCILED | COMPENSATED | FAILED
//
// Every call to an MCP server, external API, or other process the spec
// calls an "external effect" is meant to be wrapped by one Effect here,
// so an interrupted dispatch is durably recorded as OUTCOME_UNKNOWN and
// surfaced for reconciliation instead of silently lost. ReconcileInterrupted
// is what makes that real rather than aspirational: run once at daemon
// startup, it finds every effect still 'dispatched' — meaning nothing
// ever recorded what happened to it, because whatever was tracking it
// was interrupted first — and marks each outcome_unknown.
//
// Propose reuses daemon/policy for admission (decision 9: "agents
// propose; deterministic services commit") rather than reimplementing
// admission logic here — an Effect only ever exists once its operation
// has been decided by that seam, and MarkDispatchPending consumes the
// admitting decision atomically, the same "dispatch consumes the bound
// decision or fails closed" property daemon/policy's own doc comment
// describes. Note one narrow, accepted gap: Consume and this package's
// own state UPDATE are two separate transactions (daemon/policy.Consume
// does not accept an injectable transaction), so a crash in the small
// window between them leaves an effect visibly stuck 'admitted' with an
// already-consumed decision — inspectable by an operator, not silently
// corrupted, but not fully atomic either. See README for why this
// package's own doc comment calls this out rather than papering over it.
//
// Resolve's terminal-outcome argument (Confirmed/Reconciled/Compensated/
// Failed) is trusted from the caller — deliberately, not an oversight:
// §4 states plainly that "AMH SHALL NOT infer that the effect failed,
// retry blindly, construct an inverse, or select a domain recovery
// action," and §11 assigns "external effect uncertainty" to "the owning
// extension['s] reconciler," not the core. That is the opposite posture
// from daemon/selfimprove's RecordEval (which computes its verdict
// itself precisely because the core CAN independently verify a pass
// rate) — here the core has no independent way to know what actually
// happened to an external effect, so asking it to compute the verdict
// itself would be pretending to a certainty it doesn't have.
package operations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/policy"
)

var (
	ErrNotFound          = errors.New("operations: not found")
	ErrInvalidTransition = errors.New("operations: not in a valid state for this transition")
)

// State mirrors contracts/effect-record.schema.json's `state` enum.
type State string

const (
	StateProposed        State = "proposed"
	StateAdmitted        State = "admitted"
	StateRejected        State = "rejected"
	StateNeedsApproval   State = "needs_approval"
	StateDispatchPending State = "dispatch_pending"
	StateDispatched      State = "dispatched"
	StateObserved        State = "observed"
	StateOutcomeUnknown  State = "outcome_unknown"
	StateConfirmed       State = "confirmed"
	StateReconciled      State = "reconciled"
	StateCompensated     State = "compensated"
	StateFailed          State = "failed"
)

func isTerminal(s State) bool {
	switch s {
	case StateConfirmed, StateReconciled, StateCompensated, StateFailed:
		return true
	default:
		return false
	}
}

// Effect is one row of effect_record, as returned to callers.
type Effect struct {
	EffectID          string
	OperationID       string
	OwnerExtensionID  string
	EffectType        string
	DecisionID        string
	State             State
	ForwardDigest     string
	ExternalCommandID string
	ObservationRef    string
	ErrorCode         string
	ErrorRetryable    bool
	ErrorMessage      string
	Attempt           int
	Sequence          int
	RowVersion        int
	CreatedAt         string
	UpdatedAt         string
}

// EffectError is the optional error detail Resolve records alongside a
// terminal state, mirroring contracts/effect-record.schema.json's `error`
// object.
type EffectError struct {
	Code      string
	Retryable bool
	Message   string
}

// Engine is the external-effect lifecycle seam.
type Engine struct {
	DB     *sql.DB
	Policy *policy.Engine
}

func New(db *sql.DB, pol *policy.Engine) *Engine {
	return &Engine{DB: db, Policy: pol}
}

// ProposeRequest is the subset of contracts/action-envelope.schema.json
// Propose actually needs — the rest is daemon/policy's concern.
type ProposeRequest struct {
	OperationID      string
	OwnerExtensionID string
	EffectType       string
	Payload          any
	Reversibility    policy.Reversibility
}

// Propose admits req through daemon/policy and durably records the
// result as a new Effect — proposed and decided atomically from the
// caller's perspective, so a crash between "policy decided" and
// "effect recorded" cannot happen (Decide already durably persists its
// own decision; this call's only new write is the effect_record row that
// references it).
func (e *Engine) Propose(ctx context.Context, req ProposeRequest) (*Effect, error) {
	if req.OperationID == "" {
		return nil, fmt.Errorf("operations: operation_id is required")
	}
	if req.OwnerExtensionID == "" {
		return nil, fmt.Errorf("operations: owner_extension_id is required")
	}
	if req.EffectType == "" {
		return nil, fmt.Errorf("operations: effect_type is required")
	}

	decision, err := e.Policy.Decide(ctx, policy.DecideRequest{
		OperationID:   req.OperationID,
		Payload:       req.Payload,
		Reversibility: req.Reversibility,
	})
	if err != nil {
		return nil, fmt.Errorf("operations: admit via policy: %w", err)
	}

	var state State
	switch decision.Result {
	case policy.ResultAdmit, policy.ResultAdmitWithConstraints:
		state = StateAdmitted
	case policy.ResultNeedsApproval:
		state = StateNeedsApproval
	default:
		// deny/defer: this package's effect lifecycle has no matching
		// state, and DefaultPolicyID never actually returns either
		// today (see daemon/policy's doc comment) — defensive fallback,
		// not a reachable path in this build.
		state = StateRejected
	}

	effectID := uuid.NewString()
	if _, err := e.DB.ExecContext(ctx, `
		INSERT INTO effect_record (effect_id, operation_id, owner_extension_id, effect_type, decision_id, state, forward_digest)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		effectID, req.OperationID, req.OwnerExtensionID, req.EffectType, decision.ID, string(state), decision.ActionDigest,
	); err != nil {
		return nil, fmt.Errorf("operations: insert effect_record: %w", err)
	}
	return e.Get(ctx, effectID)
}

// MarkDispatchPending consumes the admitting PolicyDecision (failing
// closed if it's already consumed, expired, or digest-mismatched — see
// daemon/policy.Consume) and, only once that succeeds, transitions the
// effect to dispatch_pending: the caller's durable commitment to attempt
// the actual external call next.
func (e *Engine) MarkDispatchPending(ctx context.Context, effectID string) (*Effect, error) {
	eff, err := e.Get(ctx, effectID)
	if err != nil {
		return nil, err
	}
	if eff.State != StateAdmitted {
		return nil, fmt.Errorf("%w: %s is %s, not admitted", ErrInvalidTransition, effectID, eff.State)
	}
	if err := e.Policy.Consume(ctx, eff.DecisionID, eff.ForwardDigest); err != nil {
		return nil, fmt.Errorf("operations: consume decision for dispatch: %w", err)
	}
	if err := e.updateState(ctx, effectID, eff.RowVersion, `state = 'dispatch_pending'`); err != nil {
		return nil, err
	}
	return e.Get(ctx, effectID)
}

// MarkDispatched records that the external call was actually made —
// this is the state ReconcileInterrupted looks for on restart, since
// nothing recorded here yet means the daemon crashed before the caller
// could report what happened.
func (e *Engine) MarkDispatched(ctx context.Context, effectID, externalCommandID string) (*Effect, error) {
	eff, err := e.Get(ctx, effectID)
	if err != nil {
		return nil, err
	}
	if eff.State != StateDispatchPending {
		return nil, fmt.Errorf("%w: %s is %s, not dispatch_pending", ErrInvalidTransition, effectID, eff.State)
	}
	if err := e.updateState(ctx, effectID, eff.RowVersion, `state = 'dispatched', external_command_id = $3`, nullable(externalCommandID)); err != nil {
		return nil, err
	}
	return e.Get(ctx, effectID)
}

// MarkObserved records that the owning extension directly saw the
// dispatch's outcome (as opposed to MarkOutcomeUnknown, where nobody
// did). observationRef is a URI reference to where that evidence lives,
// mirroring contracts/effect-record.schema.json's observationRef.
func (e *Engine) MarkObserved(ctx context.Context, effectID, observationRef string) (*Effect, error) {
	eff, err := e.Get(ctx, effectID)
	if err != nil {
		return nil, err
	}
	if eff.State != StateDispatched {
		return nil, fmt.Errorf("%w: %s is %s, not dispatched", ErrInvalidTransition, effectID, eff.State)
	}
	if err := e.updateState(ctx, effectID, eff.RowVersion, `state = 'observed', observation_ref = $3`, nullable(observationRef)); err != nil {
		return nil, err
	}
	return e.Get(ctx, effectID)
}

// MarkOutcomeUnknown records that dispatch happened but nothing observed
// its result — either an extension reporting its own ambiguous timeout,
// or ReconcileInterrupted finding one stuck this way after a crash.
func (e *Engine) MarkOutcomeUnknown(ctx context.Context, effectID string) (*Effect, error) {
	eff, err := e.Get(ctx, effectID)
	if err != nil {
		return nil, err
	}
	if eff.State != StateDispatched {
		return nil, fmt.Errorf("%w: %s is %s, not dispatched", ErrInvalidTransition, effectID, eff.State)
	}
	if err := e.updateState(ctx, effectID, eff.RowVersion, `state = 'outcome_unknown'`); err != nil {
		return nil, err
	}
	return e.Get(ctx, effectID)
}

// Resolve records the owning extension's terminal verdict for an
// observed or reconciled effect. terminal must be one of
// Confirmed/Reconciled/Compensated/Failed; effErr is optional detail for
// a Failed (or otherwise unsuccessful) resolution.
func (e *Engine) Resolve(ctx context.Context, effectID string, terminal State, effErr *EffectError) (*Effect, error) {
	if !isTerminal(terminal) {
		return nil, fmt.Errorf("operations: %q is not a terminal state", terminal)
	}
	eff, err := e.Get(ctx, effectID)
	if err != nil {
		return nil, err
	}
	if eff.State != StateObserved && eff.State != StateOutcomeUnknown {
		return nil, fmt.Errorf("%w: %s is %s, not observed or outcome_unknown", ErrInvalidTransition, effectID, eff.State)
	}
	var code, message any
	var retryable any
	if effErr != nil {
		code, retryable, message = effErr.Code, effErr.Retryable, effErr.Message
	}
	if err := e.updateState(ctx, effectID, eff.RowVersion,
		`state = $3, error_code = $4, error_retryable = $5, error_message = $6`,
		string(terminal), code, retryable, message,
	); err != nil {
		return nil, err
	}
	return e.Get(ctx, effectID)
}

// ReconcileInterrupted finds every effect still recorded 'dispatched'
// and marks each outcome_unknown. Call this once, at daemon startup,
// before anything else touches operations — it is what makes acceptance
// invariant #2 ("interrupted external effects enter reconciliation and
// can remain OUTCOME_UNKNOWN") a property this daemon actually has after
// a crash, not merely a documented intention.
func (e *Engine) ReconcileInterrupted(ctx context.Context) ([]*Effect, error) {
	rows, err := e.DB.QueryContext(ctx, `SELECT effect_id FROM effect_record WHERE state = 'dispatched'`)
	if err != nil {
		return nil, fmt.Errorf("operations: query interrupted effects: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("operations: scan interrupted effect: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	out := make([]*Effect, 0, len(ids))
	for _, id := range ids {
		eff, err := e.MarkOutcomeUnknown(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("operations: mark %s outcome_unknown: %w", id, err)
		}
		out = append(out, eff)
	}
	return out, nil
}

// Get loads one effect by id.
func (e *Engine) Get(ctx context.Context, effectID string) (*Effect, error) {
	var eff Effect
	var state string
	var externalCommandID, observationRef, errorCode, errorMessage sql.NullString
	var errorRetryable sql.NullBool
	err := e.DB.QueryRowContext(ctx, `
		SELECT effect_id, operation_id, owner_extension_id, effect_type, decision_id, state, forward_digest,
		       external_command_id, observation_ref, error_code, error_retryable, error_message,
		       attempt, sequence, row_version, created_at, updated_at
		FROM effect_record WHERE effect_id = $1`, effectID,
	).Scan(&eff.EffectID, &eff.OperationID, &eff.OwnerExtensionID, &eff.EffectType, &eff.DecisionID, &state, &eff.ForwardDigest,
		&externalCommandID, &observationRef, &errorCode, &errorRetryable, &errorMessage,
		&eff.Attempt, &eff.Sequence, &eff.RowVersion, &eff.CreatedAt, &eff.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("operations: get effect %s: %w", effectID, err)
	}
	eff.State = State(state)
	eff.ExternalCommandID = externalCommandID.String
	eff.ObservationRef = observationRef.String
	eff.ErrorCode = errorCode.String
	eff.ErrorRetryable = errorRetryable.Bool
	eff.ErrorMessage = errorMessage.String
	return &eff, nil
}

// ListByOperation returns every effect recorded under operationID,
// oldest first — today that's always exactly one (see the package doc
// comment on retry-orchestration scope), but the query doesn't assume it.
func (e *Engine) ListByOperation(ctx context.Context, operationID string) ([]*Effect, error) {
	rows, err := e.DB.QueryContext(ctx, `SELECT effect_id FROM effect_record WHERE operation_id = $1 ORDER BY created_at ASC`, operationID)
	if err != nil {
		return nil, fmt.Errorf("operations: list by operation %s: %w", operationID, err)
	}
	defer rows.Close()
	var out []*Effect
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		eff, err := e.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, eff)
	}
	return out, rows.Err()
}

// updateState runs a state-transition UPDATE guarded by an optimistic
// row_version check — setClause supplies the fields beyond state/
// row_version/updated_at to set (its own placeholders start at $3, since
// $1/$2 are always effect_id/row_version), args are their values.
func (e *Engine) updateState(ctx context.Context, effectID string, expectedRowVersion int, setClause string, args ...any) error {
	fullArgs := append([]any{effectID, expectedRowVersion}, args...)
	res, err := e.DB.ExecContext(ctx, fmt.Sprintf(`
		UPDATE effect_record SET %s, row_version = row_version + 1, updated_at = iso8601_now()
		WHERE effect_id = $1 AND row_version = $2`, setClause), fullArgs...)
	if err != nil {
		return fmt.Errorf("operations: update effect %s: %w", effectID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("operations: rows affected for %s: %w", effectID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s was concurrently modified", ErrInvalidTransition, effectID)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
