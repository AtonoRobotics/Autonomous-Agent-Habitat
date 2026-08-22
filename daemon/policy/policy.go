// Package policy is AMH's generic, domain-neutral policy and approval
// seam (docs/AMH-SPECIFICATION.md §6; §2.1: "generic action admission and
// approval-hook execution"). It exists so decision 9 ("agents propose;
// deterministic services commit") is a mechanical property of the core,
// not a convention every extension has to reimplement: an extension asks
// this package to Decide on a proposed action, and — for anything that
// isn't a verified-reversible action — nothing dispatches until an
// operator resolves the resulting ApprovalRequest through Approve/Deny.
//
// This package embeds exactly one policy, DefaultPolicyID, and it is
// deliberately the only one: admit iff the proposer declares
// reversibility "verified" (an attested, currently-valid inverse exists);
// everything else needs_approval. That is the one predicate every action,
// in every domain, can be judged on without this package knowing what the
// action IS — the actual domain policy (what risk classes need what
// evidence, who may approve what) belongs to the extension, per §2.2's
// "domain policy and approval requirements", layered on top of this seam
// rather than inside it. A domain extension that needs a richer verdict
// (defer, admit_with_constraints, its own reason codes) evaluates that
// itself and calls Approve directly with its own operator identity — this
// package does not need a plugin mechanism to stay generic; it needs to
// put nothing domain-specific in the one policy it does own.
//
// Every PolicyDecision this package writes is immutable once decided:
// Consume never mutates a decision's result, and Approve never turns a
// needs_approval decision into an admit one in place. Approve mints a
// fresh, freshly-bound admit decision for the same operation/digest
// instead — see Approve's doc comment for why that, not an in-place
// update, is what "bound to the exact action digest... decision time, and
// expiry" (§6) requires.
package policy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound        = errors.New("policy: not found")
	ErrAlreadyConsumed = errors.New("policy: decision already consumed")
	ErrNotAdmitted     = errors.New("policy: only an admit or admit_with_constraints decision can be consumed")
	ErrDigestMismatch  = errors.New("policy: action_digest does not match the decision being consumed")
	ErrDecisionExpired = errors.New("policy: decision has expired")
	ErrNotPending      = errors.New("policy: approval request is not pending")
)

// Result mirrors contracts/policy-decision.schema.json's `result` enum.
type Result string

const (
	ResultAdmit                Result = "admit"
	ResultAdmitWithConstraints Result = "admit_with_constraints"
	ResultNeedsApproval        Result = "needs_approval"
	ResultDefer                Result = "defer"
	ResultDeny                 Result = "deny"
)

// Reversibility mirrors the action-envelope's properties.reversibility.status
// enum (contracts/action-envelope.schema.json) — the one generic property
// DefaultPolicyID actually looks at.
type Reversibility string

const (
	ReversibilityVerified Reversibility = "verified"
	ReversibilityClaimed  Reversibility = "claimed"
	ReversibilityNone     Reversibility = "none"
)

// DefaultPolicyID/DefaultPolicyVersion identify this package's one built-in
// policy on every decision it writes — see the package doc comment for why
// there is exactly one.
const (
	DefaultPolicyID      = "amh.core/generic-reversibility"
	DefaultPolicyVersion = "1.0.0"
)

// DefaultDecisionTTL bounds how long an admit decision may sit un-Consumed
// before it expires and must be re-Decided — short enough that a decision
// found lying around is almost certainly stale, not a queued authorization
// waiting to be used.
const DefaultDecisionTTL = 5 * time.Minute

// iso8601Format matches store/migrations' iso8601_now() output exactly
// (millisecond-precision UTC, trailing "Z") so expires_at/decided_at stay
// string-comparable and string-sortable against every other timestamp
// column in this schema, per that function's own doc comment.
const iso8601Format = "2006-01-02T15:04:05.000Z"

func iso8601(t time.Time) string { return t.UTC().Format(iso8601Format) }

// Decision is one row of policy_decision, as returned to callers.
type Decision struct {
	ID                string
	OperationID       string
	ActionDigest      string
	PolicyID          string
	PolicyVersion     string
	Result            Result
	ReasonCodes       []string
	ApprovalRequestID string
	DecidedAt         string
	ExpiresAt         string
	ConsumedAt        string
}

// ApprovalRequest is one row of approval_request, as returned to callers.
type ApprovalRequest struct {
	ID         string
	DecisionID string
	Status     string
	ResolvedBy string
	ResolvedAt string
	Reason     string
}

// Engine is the policy/approval seam.
type Engine struct {
	DB *sql.DB
}

func New(db *sql.DB) *Engine {
	return &Engine{DB: db}
}

// Digest computes the canonical action digest of payload — sha256 of its
// JSON encoding, hex-encoded and prefixed per
// contracts/action-envelope.schema.json's actionDigest/payloadDigest
// pattern ("^sha256:[a-f0-9]{64}$"). Callers (the HTTP layer, Python
// clients) use this to compute the SAME digest twice: once for the
// payload proposed to Decide, and again for the payload about to be
// dispatched when calling Consume — a mismatch there is exactly the
// "dispatching a different action than the one admitted" case Consume
// exists to catch.
//
// Deliberately relies on encoding/json's own deterministic key-sorting for
// map values rather than a hand-rolled canonicalization pass: Go's
// json.Marshal has sorted map[string]any keys lexicographically since Go
// 1.0, which is sufficient determinism for this package's purpose
// (repeatable digesting of the same logical payload), not a claim of
// general JSON Canonicalization Scheme (RFC 8785) compliance.
func Digest(payload any) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("policy: marshal payload for digest: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// DecideRequest is the subset of contracts/action-envelope.schema.json
// this package's one policy actually evaluates.
type DecideRequest struct {
	OperationID   string
	Payload       any
	Reversibility Reversibility
}

// Decide runs DefaultPolicyID against req and durably records the result
// before returning it — a decision that was computed but never persisted
// would let a crash between computing "admit" and a caller acting on it
// silently lose the fail-closed property this seam exists for.
//
// admit iff req.Reversibility is "verified"; everything else
// (unattested-but-claimed, none, or simply unset) needs_approval — fail
// closed, not fail open, on anything this generic policy cannot itself
// verify.
func (e *Engine) Decide(ctx context.Context, req DecideRequest) (*Decision, error) {
	if req.OperationID == "" {
		return nil, fmt.Errorf("policy: operation_id is required")
	}
	digest, err := Digest(req.Payload)
	if err != nil {
		return nil, err
	}

	result := ResultNeedsApproval
	reasonCodes := []string{"REVERSIBILITY_NOT_VERIFIED"}
	if req.Reversibility == ReversibilityVerified {
		result = ResultAdmit
		reasonCodes = []string{"REVERSIBILITY_VERIFIED"}
	}

	now := time.Now().UTC()
	id := uuid.NewString()
	reasonJSON, err := json.Marshal(reasonCodes)
	if err != nil {
		return nil, fmt.Errorf("policy: marshal reason_codes: %w", err)
	}

	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("policy: begin decide tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO policy_decision (id, operation_id, action_digest, policy_id, policy_version, result, reason_codes, decided_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, req.OperationID, digest, DefaultPolicyID, DefaultPolicyVersion, string(result), string(reasonJSON),
		iso8601(now), iso8601(now.Add(DefaultDecisionTTL)),
	); err != nil {
		return nil, fmt.Errorf("policy: insert decision: %w", err)
	}

	if result == ResultNeedsApproval {
		approvalID := uuid.NewString()
		if _, err := tx.ExecContext(ctx, `INSERT INTO approval_request (id, decision_id, status) VALUES ($1, $2, 'pending')`, approvalID, id); err != nil {
			return nil, fmt.Errorf("policy: insert approval_request: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE policy_decision SET approval_request_id = $1 WHERE id = $2`, approvalID, id); err != nil {
			return nil, fmt.Errorf("policy: bind approval_request_id: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("policy: commit decide: %w", err)
	}
	return e.Get(ctx, id)
}

// Consume atomically single-uses decisionID, binding it to actionDigest —
// the digest of the exact payload about to be dispatched. This is the
// mechanical enforcement of §6's "check-then-act approval is forbidden:
// dispatch consumes the bound decision atomically or fails closed": the
// row lock (FOR UPDATE) plus the consumed_at NULL check inside the same
// transaction is what makes two concurrent dispatchers racing the same
// decision result in exactly one success, not two.
//
// Fails closed on every non-happy path: not found, wrong result (deny/
// needs_approval/defer are never consumable), digest mismatch (the
// decision was for a different payload than the one about to ship),
// expired, or already consumed.
func (e *Engine) Consume(ctx context.Context, decisionID, actionDigest string) error {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("policy: begin consume tx: %w", err)
	}
	defer tx.Rollback()

	var result, digest, expiresAt string
	var consumedAt sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT result, action_digest, expires_at, consumed_at FROM policy_decision WHERE id = $1 FOR UPDATE`,
		decisionID,
	).Scan(&result, &digest, &expiresAt, &consumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("policy: query decision for consume: %w", err)
	}
	if consumedAt.Valid {
		return ErrAlreadyConsumed
	}
	if result != string(ResultAdmit) && result != string(ResultAdmitWithConstraints) {
		return ErrNotAdmitted
	}
	if digest != actionDigest {
		return ErrDigestMismatch
	}
	if expiresAt <= iso8601(time.Now()) {
		return ErrDecisionExpired
	}

	if _, err := tx.ExecContext(ctx, `UPDATE policy_decision SET consumed_at = iso8601_now() WHERE id = $1`, decisionID); err != nil {
		return fmt.Errorf("policy: mark consumed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("policy: commit consume: %w", err)
	}
	return nil
}

// Approve resolves a pending ApprovalRequest and mints a FRESH admit
// decision bound to the original decision's operation_id/action_digest —
// it does not flip the original needs_approval decision's result in
// place. An in-place update would leave the original decision's
// decided_at/expires_at describing the needs_approval verdict while its
// result field claimed admit, which breaks "the result SHALL bind the
// exact action digest, constraints, policy version, decision time, and
// expiry" (§6): the decision TIME of an approval is when the operator
// approved, not when the original policy ran. A fresh row keeps every
// PolicyDecision's fields internally consistent and keeps the audit trail
// of "policy said needs_approval, then an operator admitted it" intact
// rather than overwritten.
func (e *Engine) Approve(ctx context.Context, approvalRequestID, resolvedBy string) (*Decision, error) {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("policy: begin approve tx: %w", err)
	}
	defer tx.Rollback()

	var status, decisionID string
	err = tx.QueryRowContext(ctx, `SELECT status, decision_id FROM approval_request WHERE id = $1 FOR UPDATE`, approvalRequestID).Scan(&status, &decisionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("policy: query approval_request for approve: %w", err)
	}
	if status != "pending" {
		return nil, ErrNotPending
	}

	var operationID, actionDigest, policyID, policyVersion string
	if err := tx.QueryRowContext(ctx, `SELECT operation_id, action_digest, policy_id, policy_version FROM policy_decision WHERE id = $1`, decisionID).
		Scan(&operationID, &actionDigest, &policyID, &policyVersion); err != nil {
		return nil, fmt.Errorf("policy: query original decision: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE approval_request SET status = 'approved', resolved_by = $1, resolved_at = iso8601_now() WHERE id = $2`,
		resolvedBy, approvalRequestID); err != nil {
		return nil, fmt.Errorf("policy: resolve approval_request: %w", err)
	}

	now := time.Now().UTC()
	newID := uuid.NewString()
	reasonJSON, err := json.Marshal([]string{"APPROVED_BY_OPERATOR"})
	if err != nil {
		return nil, fmt.Errorf("policy: marshal reason_codes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO policy_decision (id, operation_id, action_digest, policy_id, policy_version, result, reason_codes, decided_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, 'admit', $6, $7, $8)`,
		newID, operationID, actionDigest, policyID, policyVersion, string(reasonJSON),
		iso8601(now), iso8601(now.Add(DefaultDecisionTTL)),
	); err != nil {
		return nil, fmt.Errorf("policy: insert approved decision: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("policy: commit approve: %w", err)
	}
	return e.Get(ctx, newID)
}

// Deny resolves a pending ApprovalRequest as denied. Unlike Approve, this
// mints no new decision — the underlying action stays permanently
// un-admitted; a caller that wants to retry proposes a new operation
// through Decide rather than re-litigating this same request.
func (e *Engine) Deny(ctx context.Context, approvalRequestID, resolvedBy, reason string) (*ApprovalRequest, error) {
	res, err := e.DB.ExecContext(ctx, `
		UPDATE approval_request SET status = 'denied', resolved_by = $1, resolved_at = iso8601_now(), reason = $2
		WHERE id = $3 AND status = 'pending'`,
		resolvedBy, reason, approvalRequestID,
	)
	if err != nil {
		return nil, fmt.Errorf("policy: deny approval_request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("policy: rows affected for deny: %w", err)
	}
	if n == 0 {
		if _, err := e.GetApprovalRequest(ctx, approvalRequestID); err != nil {
			return nil, err
		}
		return nil, ErrNotPending
	}
	return e.GetApprovalRequest(ctx, approvalRequestID)
}

// Get fetches a decision by id.
func (e *Engine) Get(ctx context.Context, id string) (*Decision, error) {
	var d Decision
	var reasonJSON string
	var approvalRequestID, consumedAt sql.NullString
	err := e.DB.QueryRowContext(ctx, `
		SELECT id, operation_id, action_digest, policy_id, policy_version, result, reason_codes, approval_request_id, decided_at, expires_at, consumed_at
		FROM policy_decision WHERE id = $1`, id,
	).Scan(&d.ID, &d.OperationID, &d.ActionDigest, &d.PolicyID, &d.PolicyVersion, &d.Result, &reasonJSON, &approvalRequestID, &d.DecidedAt, &d.ExpiresAt, &consumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("policy: get decision: %w", err)
	}
	if reasonJSON != "" {
		if err := json.Unmarshal([]byte(reasonJSON), &d.ReasonCodes); err != nil {
			return nil, fmt.Errorf("policy: unmarshal reason_codes: %w", err)
		}
	}
	d.ApprovalRequestID = approvalRequestID.String
	d.ConsumedAt = consumedAt.String
	return &d, nil
}

// GetApprovalRequest fetches an approval request by id.
func (e *Engine) GetApprovalRequest(ctx context.Context, id string) (*ApprovalRequest, error) {
	var a ApprovalRequest
	var resolvedBy, resolvedAt, reason sql.NullString
	err := e.DB.QueryRowContext(ctx, `
		SELECT id, decision_id, status, resolved_by, resolved_at, reason FROM approval_request WHERE id = $1`, id,
	).Scan(&a.ID, &a.DecisionID, &a.Status, &resolvedBy, &resolvedAt, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("policy: get approval_request: %w", err)
	}
	a.ResolvedBy = resolvedBy.String
	a.ResolvedAt = resolvedAt.String
	a.Reason = reason.String
	return &a, nil
}

// ListPendingApprovals returns every approval_request still awaiting
// operator resolution, oldest first — the queue an operator UI polls.
func (e *Engine) ListPendingApprovals(ctx context.Context) ([]ApprovalRequest, error) {
	rows, err := e.DB.QueryContext(ctx, `
		SELECT id, decision_id, status, resolved_by, resolved_at, reason FROM approval_request
		WHERE status = 'pending' ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("policy: list pending approvals: %w", err)
	}
	defer rows.Close()

	var out []ApprovalRequest
	for rows.Next() {
		var a ApprovalRequest
		var resolvedBy, resolvedAt, reason sql.NullString
		if err := rows.Scan(&a.ID, &a.DecisionID, &a.Status, &resolvedBy, &resolvedAt, &reason); err != nil {
			return nil, fmt.Errorf("policy: scan approval_request: %w", err)
		}
		a.ResolvedBy = resolvedBy.String
		a.ResolvedAt = resolvedAt.String
		a.Reason = reason.String
		out = append(out, a)
	}
	return out, rows.Err()
}
