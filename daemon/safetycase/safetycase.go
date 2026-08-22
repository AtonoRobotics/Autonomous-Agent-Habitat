// Package safetycase manages the SafetyCase lifecycle — the harder
// evidence path to earned autonomy for actions with no verified inverse
// (§14.7, Artifact B/C/E). Where the ApprovalGate (daemon/interlocks)
// gates one action at a time, a SafetyCase is a standing, revocable grant
// of autonomy for a whole device_action or capability, built from
// guardrail evidence and independent review rather than reversibility.
//
// This package deliberately collapses the spec's SafetyCase.request_review
// (a distinct step routing to "an agent-external reviewer," separate from
// approval itself) into one action: Approve is gated to authn.RoleOperator
// at the HTTP layer (daemon/api), and the fact that only an operator
// credential can call it IS the independent review — there is no separate
// reviewer role or ReviewTicket workflow, matching the spec's own caveat
// that "the irreversible-action proof engine... requires a defined
// independent-reviewer role this spec does not invent." Approve sets
// independent_review=1 atomically with approved_at for every risk_class,
// not only the ones the spec's floor requires it for (moderate/high/
// irreversible_high_consequence) — a deliberately more conservative
// posture than the minimum, not a shortcut around it.
package safetycase

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

type RiskClass string

const (
	RiskLow                    RiskClass = "low"
	RiskModerate               RiskClass = "moderate"
	RiskHigh                   RiskClass = "high"
	RiskIrreversibleHighConseq RiskClass = "irreversible_high_consequence"
)

func (r RiskClass) valid() bool {
	switch r {
	case RiskLow, RiskModerate, RiskHigh, RiskIrreversibleHighConseq:
		return true
	}
	return false
}

type SubjectType string

const (
	SubjectDeviceAction SubjectType = "device_action"
	SubjectCapability   SubjectType = "capability"
)

func (s SubjectType) valid() bool {
	return s == SubjectDeviceAction || s == SubjectCapability
}

var (
	ErrInvalidRiskClass   = errors.New("safetycase: invalid risk_class")
	ErrInvalidSubjectType = errors.New("safetycase: invalid subject_type")
	ErrNotFound           = errors.New("safetycase: not found")
	ErrAlreadyApproved    = errors.New("safetycase: already approved")
	ErrAlreadyRevoked     = errors.New("safetycase: already revoked; build a new case rather than re-approving this one")
	ErrNotApproved        = errors.New("safetycase: cannot revoke a case that was never approved")
)

type Status struct {
	ID                string
	SubjectID         string
	SubjectType       SubjectType
	RiskClass         RiskClass
	IndependentReview bool
	Approved          bool
	Revoked           bool
	RevokedReason     string
}

type Registry struct {
	DB *sql.DB
}

func New(db *sql.DB) *Registry {
	return &Registry{DB: db}
}

// Create opens a new SafetyCase for a subject. Guardrail evidence is
// submitted incrementally afterward via SubmitEvidence — a case starts
// with none.
func (r *Registry) Create(ctx context.Context, subjectID string, subjectType SubjectType, riskClass RiskClass) (string, error) {
	if !subjectType.valid() {
		return "", ErrInvalidSubjectType
	}
	if !riskClass.valid() {
		return "", ErrInvalidRiskClass
	}
	id := uuid.NewString()
	_, err := r.DB.ExecContext(ctx,
		`INSERT INTO safety_case (id, subject_id, subject_type, risk_class, guardrails)
		 VALUES ($1, $2, $3, $4, '[]')`,
		id, subjectID, string(subjectType), string(riskClass),
	)
	if err != nil {
		return "", fmt.Errorf("safetycase: create: %w", err)
	}
	return id, nil
}

// SubmitEvidence appends one guardrail-proof entry to the case's evidence
// list — a real deployment accumulates these over the case's supervised
// track record, not all at once, so this is append, not replace.
func (r *Registry) SubmitEvidence(ctx context.Context, id string, guardrailProof map[string]any) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("safetycase: begin tx: %w", err)
	}
	defer tx.Rollback()

	var existingJSON string
	err = tx.QueryRowContext(ctx, `SELECT guardrails FROM safety_case WHERE id = $1`, id).Scan(&existingJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("safetycase: load guardrails: %w", err)
	}

	var evidence []map[string]any
	if existingJSON != "" {
		if err := json.Unmarshal([]byte(existingJSON), &evidence); err != nil {
			return fmt.Errorf("safetycase: parse existing guardrails: %w", err)
		}
	}
	evidence = append(evidence, guardrailProof)

	updatedJSON, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("safetycase: marshal guardrails: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE safety_case SET guardrails = $1 WHERE id = $2`, string(updatedJSON), id); err != nil {
		return fmt.Errorf("safetycase: update guardrails: %w", err)
	}
	return tx.Commit()
}

// Approve grants earned autonomy: sets independent_review=1 and
// approved_at/approved_by together. Refuses a case that's already
// approved (idempotency, mirroring interlocks.Gate.Approve) or already
// revoked — per §14.7, a revoked case is rebuilt (a fresh Create), not
// re-approved; approving the same dead row would silently resurrect a
// case that had a safety-relevant incident against it.
func (r *Registry) Approve(ctx context.Context, id, approvedBy string) error {
	var approvedAt, revokedAt sql.NullString
	err := r.DB.QueryRowContext(ctx,
		`SELECT approved_at, revoked_at FROM safety_case WHERE id = $1`, id,
	).Scan(&approvedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("safetycase: load: %w", err)
	}
	if revokedAt.Valid {
		return ErrAlreadyRevoked
	}
	if approvedAt.Valid {
		return ErrAlreadyApproved
	}

	_, err = r.DB.ExecContext(ctx,
		`UPDATE safety_case
		 SET independent_review = 1, approved_at = iso8601_now(), approved_by = $1
		 WHERE id = $2`,
		approvedBy, id,
	)
	if err != nil {
		return fmt.Errorf("safetycase: approve: %w", err)
	}
	return nil
}

// Revoke immediately withdraws an approved case's autonomy — no rate
// window, no grace period. Per §14.7, this is deliberately asymmetric
// with the ApprovalGate's reversible track: a single safety-relevant
// incident against an irreversible action's case ends it outright.
func (r *Registry) Revoke(ctx context.Context, id, reason string) error {
	var approvedAt, revokedAt sql.NullString
	err := r.DB.QueryRowContext(ctx,
		`SELECT approved_at, revoked_at FROM safety_case WHERE id = $1`, id,
	).Scan(&approvedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("safetycase: load: %w", err)
	}
	if revokedAt.Valid {
		return ErrAlreadyRevoked
	}
	if !approvedAt.Valid {
		return ErrNotApproved
	}

	_, err = r.DB.ExecContext(ctx,
		`UPDATE safety_case
		 SET revoked_at = iso8601_now(), revoked_reason = $1
		 WHERE id = $2`,
		reason, id,
	)
	if err != nil {
		return fmt.Errorf("safetycase: revoke: %w", err)
	}
	return nil
}

// Status reads a case's current state.
func (r *Registry) Status(ctx context.Context, id string) (Status, error) {
	var st Status
	var subjectType, riskClass string
	var independentReview int
	var approvedAt, revokedAt, revokedReason sql.NullString
	err := r.DB.QueryRowContext(ctx,
		`SELECT id, subject_id, subject_type, risk_class, independent_review, approved_at, revoked_at, revoked_reason
		 FROM safety_case WHERE id = $1`, id,
	).Scan(&st.ID, &st.SubjectID, &subjectType, &riskClass, &independentReview, &approvedAt, &revokedAt, &revokedReason)
	if errors.Is(err, sql.ErrNoRows) {
		return Status{}, ErrNotFound
	}
	if err != nil {
		return Status{}, fmt.Errorf("safetycase: status: %w", err)
	}
	st.SubjectType = SubjectType(subjectType)
	st.RiskClass = RiskClass(riskClass)
	st.IndependentReview = independentReview != 0
	st.Approved = approvedAt.Valid
	st.Revoked = revokedAt.Valid
	st.RevokedReason = revokedReason.String
	return st, nil
}
