package extensions

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

var (
	ErrKeyNotFound      = errors.New("extensions: trusted signing key not found")
	ErrKeyRevoked       = errors.New("extensions: trusted signing key has been revoked")
	ErrKeyExists        = errors.New("extensions: a trusted signing key with this key_id already exists")
	ErrInvalidPublicKey = errors.New("extensions: public_key must be 32 hex-encoded bytes (an Ed25519 public key)")
)

// TrustedKey is one operator-registered Ed25519 public key, identified by
// an operator-chosen keyId that a manifest's spec.signature.keyId
// references.
type TrustedKey struct {
	KeyID        string
	PublicKeyHex string
	CreatedAt    string
	RevokedAt    string
}

// TrustStore is the durable, operator-managed set of signing keys Discover
// consults to admit a signed extension manifest. Key material is immutable
// once registered — rotate by registering a new keyId and revoking the
// old one, never by overwriting a keyId already in use.
type TrustStore struct {
	DB *sql.DB
}

func newTrustStore(db *sql.DB) *TrustStore { return &TrustStore{DB: db} }

// RegisterKey trusts publicKeyHex (32 raw Ed25519 public-key bytes,
// hex-encoded) under keyId. Fails if keyId is already registered, revoked
// or not — a revoked keyId can never be reused, so a genuine rotation
// always mints a fresh keyId.
func (t *TrustStore) RegisterKey(ctx context.Context, keyID, publicKeyHex string) (*TrustedKey, error) {
	if _, err := decodeEd25519PublicKey(publicKeyHex); err != nil {
		return nil, err
	}
	if _, err := t.Get(ctx, keyID); err == nil {
		return nil, ErrKeyExists
	} else if !errors.Is(err, ErrKeyNotFound) {
		return nil, err
	}
	if _, err := t.DB.ExecContext(ctx, `INSERT INTO trusted_signing_key (key_id, public_key) VALUES ($1, $2)`, keyID, publicKeyHex); err != nil {
		return nil, fmt.Errorf("extensions: register trusted key %s: %w", keyID, err)
	}
	return t.Get(ctx, keyID)
}

// RevokeKey marks a trusted key revoked. A revoked key can no longer admit
// a signed manifest through Discover, but any extension already discovered
// or activated under it is untouched — revocation is forward-only, the
// same posture daemon/credentials.RevokeAccount takes toward its accounts.
func (t *TrustStore) RevokeKey(ctx context.Context, keyID string) (*TrustedKey, error) {
	res, err := t.DB.ExecContext(ctx, `UPDATE trusted_signing_key SET revoked_at = iso8601_now() WHERE key_id = $1 AND revoked_at IS NULL`, keyID)
	if err != nil {
		return nil, fmt.Errorf("extensions: revoke trusted key %s: %w", keyID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, getErr := t.Get(ctx, keyID); getErr != nil {
			return nil, getErr
		}
		return nil, fmt.Errorf("%w: %s", ErrKeyRevoked, keyID)
	}
	return t.Get(ctx, keyID)
}

// Get loads one trusted key by id, found or not, revoked or not.
func (t *TrustStore) Get(ctx context.Context, keyID string) (*TrustedKey, error) {
	var k TrustedKey
	var revokedAt sql.NullString
	err := t.DB.QueryRowContext(ctx, `SELECT key_id, public_key, created_at, revoked_at FROM trusted_signing_key WHERE key_id = $1`, keyID).
		Scan(&k.KeyID, &k.PublicKeyHex, &k.CreatedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrKeyNotFound, keyID)
	}
	if err != nil {
		return nil, fmt.Errorf("extensions: get trusted key %s: %w", keyID, err)
	}
	k.RevokedAt = revokedAt.String
	return &k, nil
}

// List returns every registered trusted key, newest first.
func (t *TrustStore) List(ctx context.Context) ([]*TrustedKey, error) {
	rows, err := t.DB.QueryContext(ctx, `SELECT key_id FROM trusted_signing_key ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("extensions: list trusted keys: %w", err)
	}
	defer rows.Close()
	var out []*TrustedKey
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		k, err := t.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// verified reports whether keyID is registered and not revoked, returning
// its decoded public key for signature verification. Discover calls this
// rather than trusting anything about the key the manifest itself claims.
func (t *TrustStore) verified(ctx context.Context, keyID string) (ed25519.PublicKey, error) {
	k, err := t.Get(ctx, keyID)
	if err != nil {
		return nil, err
	}
	if k.RevokedAt != "" {
		return nil, fmt.Errorf("%w: %s", ErrKeyRevoked, keyID)
	}
	return decodeEd25519PublicKey(k.PublicKeyHex)
}

func decodeEd25519PublicKey(hexStr string) (ed25519.PublicKey, error) {
	raw, err := hex.DecodeString(hexStr)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, ErrInvalidPublicKey
	}
	return ed25519.PublicKey(raw), nil
}
