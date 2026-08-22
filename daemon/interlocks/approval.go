// Package interlocks enforces the ApprovalGate: the single point where an
// action with no verified inverse and no approved SafetyCase is blocked
// until a human (or other agent-external authority) approves it. See
// docs/AMH-SPECIFICATION.md §12 and Artifact B (ApprovalGate protocol).
//
// Reversibility, not physicality, is the sole gating axis (v6). This gate
// exists to cover exactly the residue with no verified inverse and no
// approved SafetyCase — not a blanket check on every action.
package interlocks

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Risk mirrors the Python ApprovalGate.require() risk parameter.
type Risk string

const (
	Reversible   Risk = "reversible"
	Irreversible Risk = "irreversible"
)

// Ticket references a pending or resolved approval_gate row.
type Ticket struct {
	ID string
}

var (
	// ErrNotSatisfied is returned by Enforce when a ticket has not yet
	// been approved — the caller must not proceed with the gated action.
	ErrNotSatisfied = errors.New("interlocks: approval not satisfied")
	// ErrActionMismatch is returned by Enforce when the ticket was
	// approved for a different device_action or a different set of
	// parameters than the one actually being requested — an approved
	// ticket authorizes only the exact action it was requested for.
	ErrActionMismatch = errors.New("interlocks: ticket does not match the requested action")
	// ErrTicketAlreadyUsed is returned by Enforce on a ticket that has
	// already been consumed by a prior successful Enforce call — a
	// ticket authorizes exactly one actuation, not unlimited replays.
	ErrTicketAlreadyUsed = errors.New("interlocks: ticket has already been used")
)

type Gate struct {
	DB *sql.DB
}

func New(db *sql.DB) *Gate {
	return &Gate{DB: db}
}

// actionDigest is a deterministic fingerprint of exactly what a ticket
// covers: one device_action, with one specific set of rendered
// parameters. Require stores it at approval-request time; Enforce
// recomputes it from the actual actuation request and refuses to proceed
// on a mismatch — an approved ticket authorizes only the one action it
// was requested for, never a substitute. json.Marshal of a Go map is
// deterministic (keys sorted), so the same (deviceActionID, params) pair
// always produces the same digest regardless of map iteration order.
func actionDigest(deviceActionID string, params map[string]string) string {
	payload, _ := json.Marshal(struct {
		DeviceActionID string            `json:"device_action_id"`
		Params         map[string]string `json:"params"`
	}{deviceActionID, params})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// Require persists a new approval_gate row scoped to one device action and
// its exact parameters, and returns a ticket. reason is free-text audit
// context only (e.g. "scheduled feeding") — it has no bearing on
// action_digest, so it never affects what Enforce will accept. It does
// not block — approval is granted out-of-band (an operator, or later an
// agent-external reviewer for a SafetyCase) by calling Approve with the
// same ticket ID.
func (g *Gate) Require(ctx context.Context, deviceActionID string, params map[string]string, reason string, risk Risk) (Ticket, error) {
	digest := actionDigest(deviceActionID, params)
	actionJSON, err := json.Marshal(struct {
		DeviceActionID string            `json:"device_action_id"`
		Params         map[string]string `json:"params"`
		Reason         string            `json:"reason,omitempty"`
	}{deviceActionID, params, reason})
	if err != nil {
		return Ticket{}, fmt.Errorf("interlocks: marshal action: %w", err)
	}
	id := uuid.NewString()
	_, err = g.DB.ExecContext(ctx,
		`INSERT INTO approval_gate (id, action, risk, action_digest) VALUES ($1, $2, $3, $4)`,
		id, string(actionJSON), string(risk), digest,
	)
	if err != nil {
		return Ticket{}, fmt.Errorf("interlocks: insert approval_gate: %w", err)
	}
	return Ticket{ID: id}, nil
}

// Approve records approval for a pending ticket. approvedBy identifies the
// agent-external authority (a human operator ID, an audit-service ID —
// never the requesting agent itself).
func (g *Gate) Approve(ctx context.Context, ticket Ticket, approvedBy string) error {
	res, err := g.DB.ExecContext(ctx,
		`UPDATE approval_gate SET approved_by = $1, approved_at = iso8601_now()
		 WHERE id = $2 AND approved_at IS NULL`,
		approvedBy, ticket.ID,
	)
	if err != nil {
		return fmt.Errorf("interlocks: approve: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("interlocks: approve rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("interlocks: ticket %s not found or already approved", ticket.ID)
	}
	return nil
}

// IsSatisfied reports whether a ticket has been approved.
func (g *Gate) IsSatisfied(ctx context.Context, ticket Ticket) (bool, error) {
	var approvedAt sql.NullString
	err := g.DB.QueryRowContext(ctx,
		`SELECT approved_at FROM approval_gate WHERE id = $1`, ticket.ID,
	).Scan(&approvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("interlocks: ticket %s not found", ticket.ID)
	}
	if err != nil {
		return false, fmt.Errorf("interlocks: query ticket: %w", err)
	}
	return approvedAt.Valid, nil
}

// Enforce checks that ticket has been approved for exactly this
// (deviceActionID, params) action — not merely approved for something —
// and atomically consumes it (single-use) so it cannot be replayed for a
// later, unrelated actuation. Callers use this immediately before
// executing a gated action; the caller must pass the actual action about
// to run, not a value it merely hopes matches what was approved.
func (g *Gate) Enforce(ctx context.Context, ticket Ticket, deviceActionID string, params map[string]string) error {
	var approvedAt, usedAt sql.NullString
	var storedDigest string
	err := g.DB.QueryRowContext(ctx,
		`SELECT approved_at, used_at, action_digest FROM approval_gate WHERE id = $1`, ticket.ID,
	).Scan(&approvedAt, &usedAt, &storedDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("interlocks: ticket %s not found", ticket.ID)
	}
	if err != nil {
		return fmt.Errorf("interlocks: query ticket: %w", err)
	}
	if !approvedAt.Valid {
		return fmt.Errorf("%w: ticket %s", ErrNotSatisfied, ticket.ID)
	}
	if usedAt.Valid {
		return fmt.Errorf("%w: ticket %s", ErrTicketAlreadyUsed, ticket.ID)
	}
	if storedDigest != actionDigest(deviceActionID, params) {
		return fmt.Errorf("%w: ticket %s was approved for a different action", ErrActionMismatch, ticket.ID)
	}

	res, err := g.DB.ExecContext(ctx,
		`UPDATE approval_gate SET used_at = iso8601_now() WHERE id = $1 AND used_at IS NULL`,
		ticket.ID,
	)
	if err != nil {
		return fmt.Errorf("interlocks: consume ticket: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("interlocks: consume ticket rows affected: %w", err)
	}
	if n == 0 {
		// Lost a race with a concurrent Enforce call between the SELECT
		// above and this UPDATE — the other caller consumed it first.
		return fmt.Errorf("%w: ticket %s", ErrTicketAlreadyUsed, ticket.ID)
	}
	return nil
}
