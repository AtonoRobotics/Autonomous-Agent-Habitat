package extensions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

var (
	// ErrOutstandingContextEffects is returned by Dispose when the
	// extension being disposed has registered one or more core-mediated
	// effects (RegisterContextEffect) it has not yet disposed
	// (DisposeContextEffect) — §15 acceptance invariant #3 / §5.1's
	// "effects SHALL be disposed in reverse registration order" made a
	// fail-closed precondition on Dispose itself, rather than a rule an
	// extension could simply ignore.
	ErrOutstandingContextEffects = errors.New("extensions: extension still has undisposed core-mediated effects")

	// ErrNotMostRecentlyRegistered is returned by DisposeContextEffect
	// when effectID is not the most recently registered, still-
	// outstanding effect for its extension instance — §5.1's "reverse
	// registration order" requirement, enforced mechanically: an
	// extension cannot dispose effect N-1 while effect N is still
	// outstanding.
	ErrNotMostRecentlyRegistered = errors.New("extensions: only the most recently registered, still-outstanding effect can be disposed next (reverse registration order)")
)

// ContextEffect is one core-mediated effect an active extension has
// self-reported creating — a tool registration, event subscription,
// timer, route, service publication, or child-extension mount per
// §5.1's examples, though kind/ref are free-form and opaque to core:
// core cannot execute an arbitrary extension's disposer itself (the
// same reason §4 says core "SHALL NOT construct an inverse" for
// external effects), so what it enforces is order and completeness of
// the extension's own self-reported registration/disposal, not the
// disposal mechanics themselves.
type ContextEffect struct {
	ID       string
	Sequence int
	Kind     string
	Ref      string
	Disposed bool
}

// RegisterContextEffect records that extension id@version has just
// created one core-mediated effect (kind, ref) — called by the
// extension itself, at the moment the effect is created, per §5.1
// ("SHALL register its disposer at the time the effect is created").
// Requires the extension to be currently active: a not-yet-activated or
// already-disposed extension has no runtime context in which to have
// created anything core-mediated.
func (r *Registry) RegisterContextEffect(ctx context.Context, id, version, kind, ref string) (*ContextEffect, error) {
	ext, err := r.Get(ctx, id, version)
	if err != nil {
		return nil, err
	}
	if ext.Status != StatusActive {
		return nil, fmt.Errorf("%w: %s@%s is %s, not active", ErrInvalidState, id, version, ext.Status)
	}

	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("extensions: begin register context effect tx: %w", err)
	}
	defer tx.Rollback()

	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(sequence) FROM extension_context_effect WHERE extension_id = $1 AND extension_version = $2`,
		id, version,
	).Scan(&maxSeq); err != nil {
		return nil, fmt.Errorf("extensions: query max sequence: %w", err)
	}
	sequence := int(maxSeq.Int64) + 1

	effectID := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO extension_context_effect (id, extension_id, extension_version, sequence, kind, ref)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		effectID, id, version, sequence, kind, ref,
	); err != nil {
		return nil, fmt.Errorf("extensions: insert context effect: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("extensions: commit register context effect: %w", err)
	}

	return &ContextEffect{ID: effectID, Sequence: sequence, Kind: kind, Ref: ref}, nil
}

// DisposeContextEffect records that the extension has torn down one
// core-mediated effect it previously registered. Fails closed with
// ErrNotMostRecentlyRegistered unless effectID is the most recently
// registered effect for its extension instance that is still
// outstanding — mechanically enforcing §5.1's "disposed in reverse
// registration order" rather than trusting the caller to self-police it.
func (r *Registry) DisposeContextEffect(ctx context.Context, id, version, effectID string) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("extensions: begin dispose context effect tx: %w", err)
	}
	defer tx.Rollback()

	var sequence int
	var disposedAt sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT sequence, disposed_at FROM extension_context_effect
		WHERE id = $1 AND extension_id = $2 AND extension_version = $3 FOR UPDATE`,
		effectID, id, version,
	).Scan(&sequence, &disposedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("extensions: query context effect: %w", err)
	}
	if disposedAt.Valid {
		return fmt.Errorf("%w: %s is already disposed", ErrInvalidState, effectID)
	}

	var maxOutstandingSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(sequence) FROM extension_context_effect
		WHERE extension_id = $1 AND extension_version = $2 AND disposed_at IS NULL`,
		id, version,
	).Scan(&maxOutstandingSeq); err != nil {
		return fmt.Errorf("extensions: query max outstanding sequence: %w", err)
	}
	if !maxOutstandingSeq.Valid || int(maxOutstandingSeq.Int64) != sequence {
		return ErrNotMostRecentlyRegistered
	}

	if _, err := tx.ExecContext(ctx, `UPDATE extension_context_effect SET disposed_at = iso8601_now() WHERE id = $1`, effectID); err != nil {
		return fmt.Errorf("extensions: mark context effect disposed: %w", err)
	}
	return tx.Commit()
}

// ListOutstandingContextEffects returns every core-mediated effect id@version
// has registered and not yet disposed, oldest (lowest sequence) first —
// what Dispose checks is empty, and what an operator would see explaining
// a refused Dispose.
func (r *Registry) ListOutstandingContextEffects(ctx context.Context, id, version string) ([]ContextEffect, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, sequence, kind, ref FROM extension_context_effect
		WHERE extension_id = $1 AND extension_version = $2 AND disposed_at IS NULL
		ORDER BY sequence ASC`,
		id, version,
	)
	if err != nil {
		return nil, fmt.Errorf("extensions: list outstanding context effects: %w", err)
	}
	defer rows.Close()

	var out []ContextEffect
	for rows.Next() {
		var e ContextEffect
		if err := rows.Scan(&e.ID, &e.Sequence, &e.Kind, &e.Ref); err != nil {
			return nil, fmt.Errorf("extensions: scan context effect: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
