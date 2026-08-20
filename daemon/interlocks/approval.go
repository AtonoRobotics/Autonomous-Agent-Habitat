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
	"database/sql"
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

// ErrNotSatisfied is returned by Enforce when a ticket has not yet been
// approved — the caller must not proceed with the gated action.
var ErrNotSatisfied = errors.New("interlocks: approval not satisfied")

type Gate struct {
	DB *sql.DB
}

func New(db *sql.DB) *Gate {
	return &Gate{DB: db}
}

// Require persists a new approval_gate row for the given action and
// returns a ticket. It does not block — approval is granted out-of-band
// (an operator, or later an agent-external reviewer for a SafetyCase) by
// calling Approve with the same ticket ID.
func (g *Gate) Require(ctx context.Context, action any, risk Risk) (Ticket, error) {
	actionJSON, err := json.Marshal(action)
	if err != nil {
		return Ticket{}, fmt.Errorf("interlocks: marshal action: %w", err)
	}
	id := uuid.NewString()
	_, err = g.DB.ExecContext(ctx,
		`INSERT INTO approval_gate (id, action, risk) VALUES (?, ?, ?)`,
		id, string(actionJSON), string(risk),
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
		`UPDATE approval_gate SET approved_by = ?, approved_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ? AND approved_at IS NULL`,
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
		`SELECT approved_at FROM approval_gate WHERE id = ?`, ticket.ID,
	).Scan(&approvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("interlocks: ticket %s not found", ticket.ID)
	}
	if err != nil {
		return false, fmt.Errorf("interlocks: query ticket: %w", err)
	}
	return approvedAt.Valid, nil
}

// Enforce is a convenience that returns ErrNotSatisfied unless the ticket
// has been approved — callers use this immediately before executing a
// gated action.
func (g *Gate) Enforce(ctx context.Context, ticket Ticket) error {
	ok, err := g.IsSatisfied(ctx, ticket)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: ticket %s", ErrNotSatisfied, ticket.ID)
	}
	return nil
}
