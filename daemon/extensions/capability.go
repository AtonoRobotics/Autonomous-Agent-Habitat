package extensions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// mintCapabilityToken generates a fresh, random bearer token for one
// activated extension instance and durably records its hash — never the
// raw token, the same "never persist a secret unencrypted" posture
// daemon/credentials already takes for account secrets, though a hash
// rather than reversible encryption here since this token is never read
// back, only compared against. Called once per Activate (see
// registry.go), replacing any token from a previous activation of the
// same id/version via an upsert: a new process instance gets a new
// token, and an old, no-longer-current instance's token stops verifying
// the moment this runs — before Dispose is ever called on it.
//
// SHA-256, looked up by exact hash match through an indexed column, not
// compared byte-by-byte in Go: unlike daemon/authn's two long-lived,
// externally-guessable-by-repeated-attempts static role tokens (which
// need subtle.ConstantTimeCompare to close a timing side channel), a
// database index lookup by a cryptographic hash isn't a meaningful
// timing oracle — recovering a SHA-256 preimage from lookup timing isn't
// a practical attack.
func mintCapabilityToken(ctx context.Context, db *sql.DB, id, version string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("extensions: generate capability token: %w", err)
	}
	token := hex.EncodeToString(raw)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO extension_capability_token (extension_id, extension_version, token_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (extension_id, extension_version) DO UPDATE SET token_hash = EXCLUDED.token_hash, created_at = iso8601_now()`,
		id, version, hashToken(token),
	); err != nil {
		return "", fmt.Errorf("extensions: store capability token: %w", err)
	}
	return token, nil
}

// revokeCapabilityToken deletes id/version's current token, if any —
// called from Dispose. Idempotent: disposing an in_process extension
// (which never had a token minted) or an extension whose token row is
// already gone is a harmless no-op, not an error.
func revokeCapabilityToken(ctx context.Context, db *sql.DB, id, version string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM extension_capability_token WHERE extension_id = $1 AND extension_version = $2`, id, version)
	if err != nil {
		return fmt.Errorf("extensions: revoke capability token: %w", err)
	}
	return nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// VerifyCapabilityToken reports which currently-active extension
// instance a capability token belongs to, if any — ok is false for an
// empty, unrecognized, or since-revoked/replaced token, never an error;
// err is reserved for a real infrastructure failure (a query that
// couldn't run at all). Returns both id and version, not just id: two
// different versions of the same extension id can be simultaneously
// active (nothing in Activate prevents it), each with its own distinct
// token, so id alone would be ambiguous for a caller that needs to
// authorize against one specific instance (see
// daemon/api/controlplane.go's context-effect routes) — callers that
// only ever needed id (daemon/api/operations.go's authorizeEffectOwner,
// since effect_record.owner_extension_id carries no version) simply
// ignore the extra return value.
func (r *Registry) VerifyCapabilityToken(ctx context.Context, token string) (extensionID, extensionVersion string, ok bool, err error) {
	if token == "" {
		return "", "", false, nil
	}
	var id, version string
	err = r.DB.QueryRowContext(ctx, `SELECT extension_id, extension_version FROM extension_capability_token WHERE token_hash = $1`, hashToken(token)).Scan(&id, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("extensions: verify capability token: %w", err)
	}
	return id, version, true, nil
}
