// Package credentials is the encrypted-at-rest secret store for
// "authenticate accounts and modules": external service accounts a
// connector or extension needs to act as (a GitHub account, an SMTP
// login), and module-level secrets an extension itself needs (an API key
// its own code calls out with). One store, one encryption discipline,
// serving all three credential subjects (account, connector, extension)
// uniformly — see store/migrations/0002_control_plane.sql's credential
// table.
//
// Same fail-closed posture as daemon/authn: the encryption key comes from
// an environment variable, and a missing or malformed key is a
// configuration error this package refuses to silently work around by,
// say, deriving a key from something guessable. See LoadKeyFromEnv.
package credentials

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"
)

type SubjectType string

const (
	SubjectAccount   SubjectType = "account"
	SubjectConnector SubjectType = "connector"
	SubjectExtension SubjectType = "extension"
)

type AccountStatus string

const (
	AccountPending AccountStatus = "pending"
	AccountActive  AccountStatus = "active"
	AccountRevoked AccountStatus = "revoked"
)

var (
	ErrKeyRequired    = errors.New("credentials: AMH_CREDENTIAL_KEY is required")
	ErrInvalidKey     = errors.New("credentials: AMH_CREDENTIAL_KEY must decode (base64) to exactly 32 bytes")
	ErrNotFound       = errors.New("credentials: not found")
	ErrNoCredential   = errors.New("credentials: subject has no active credential")
	ErrInvalidSubject = errors.New("credentials: subject_type must be account, connector, or extension")
)

// LoadKeyFromEnv reads and decodes AMH_CREDENTIAL_KEY (base64 standard
// encoding of exactly 32 raw bytes — an AES-256 key). Fails closed: no
// fallback, no key derivation from a weaker secret. Generate one with
// `openssl rand -base64 32`.
func LoadKeyFromEnv() ([]byte, error) {
	encoded := os.Getenv("AMH_CREDENTIAL_KEY")
	if encoded == "" {
		return nil, ErrKeyRequired
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, ErrInvalidKey
	}
	return key, nil
}

// keyID is a non-secret fingerprint of the key, recorded on every
// credential row so a future key rotation can identify which rows need
// re-encryption without ever storing or logging the key itself.
func keyID(key []byte) string {
	sum := sha256.Sum256(key)
	return "k_" + base64.RawURLEncoding.EncodeToString(sum[:8])
}

// Store is the credential vault. New fails closed if key is not exactly
// 32 bytes (AES-256) — see LoadKeyFromEnv.
type Store struct {
	DB    *sql.DB
	key   []byte
	keyID string
}

func New(db *sql.DB, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKey
	}
	return &Store{DB: db, key: key, keyID: keyID(key)}, nil
}

// Account is metadata only — never a secret. Credential material for an
// account lives exclusively in the credential table, reachable only via
// Put/Authenticate below.
type Account struct {
	ID          string
	Provider    string
	DisplayName string
	Status      AccountStatus
}

// CreateAccount registers a new account shell (no credential yet) —
// status starts "pending" until a credential is stored via PutCredential,
// which activates it.
func (s *Store) CreateAccount(ctx context.Context, provider, displayName string) (*Account, error) {
	if provider == "" {
		return nil, fmt.Errorf("credentials: provider is required")
	}
	id := uuid.NewString()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO account (id, provider, display_name, status) VALUES (?, ?, ?, 'pending')`, id, provider, displayName)
	if err != nil {
		return nil, fmt.Errorf("credentials: insert account: %w", err)
	}
	return s.GetAccount(ctx, id)
}

// RevokeAccount marks an account revoked and revokes its active
// credential (if any) in the same operation — an account cannot be
// revoked while still holding a live credential that Authenticate would
// return.
func (s *Store) RevokeAccount(ctx context.Context, id string) (*Account, error) {
	acct, err := s.GetAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	if acct.Status == AccountRevoked {
		return acct, nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("credentials: begin revoke tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE account SET status = 'revoked', revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("credentials: revoke account: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE credential SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE subject_type = 'account' AND subject_id = ? AND revoked_at IS NULL`,
		id,
	); err != nil {
		return nil, fmt.Errorf("credentials: revoke account credentials: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("credentials: commit revoke: %w", err)
	}
	return s.GetAccount(ctx, id)
}

func (s *Store) GetAccount(ctx context.Context, id string) (*Account, error) {
	var a Account
	var displayName sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT id, provider, display_name, status FROM account WHERE id = ?`, id).
		Scan(&a.ID, &a.Provider, &displayName, &a.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: account %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("credentials: get account %s: %w", id, err)
	}
	a.DisplayName = displayName.String
	return &a, nil
}

func (s *Store) ListAccounts(ctx context.Context) ([]*Account, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM account ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("credentials: list accounts: %w", err)
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		a, err := s.GetAccount(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PutCredential encrypts plaintext and stores it as the new active
// credential for (subjectType, subjectID), rotating out (marking
// rotated_at on) whatever credential was previously active for that
// subject. If subjectType is "account", the account transitions to
// "active" — this is what "authenticate an account" actually does.
// plaintext is never returned by this package except via Authenticate,
// and never logged.
func (s *Store) PutCredential(ctx context.Context, subjectType SubjectType, subjectID string, plaintext []byte) (string, error) {
	if subjectType != SubjectAccount && subjectType != SubjectConnector && subjectType != SubjectExtension {
		return "", ErrInvalidSubject
	}
	if subjectID == "" {
		return "", fmt.Errorf("credentials: subject_id is required")
	}
	ciphertext, err := s.encrypt(plaintext)
	if err != nil {
		return "", err
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("credentials: begin put tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE credential SET rotated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE subject_type = ? AND subject_id = ? AND revoked_at IS NULL AND rotated_at IS NULL`,
		string(subjectType), subjectID,
	); err != nil {
		return "", fmt.Errorf("credentials: rotate previous credential: %w", err)
	}

	id := uuid.NewString()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO credential (id, subject_type, subject_id, ciphertext, key_id) VALUES (?, ?, ?, ?, ?)`,
		id, string(subjectType), subjectID, ciphertext, s.keyID,
	); err != nil {
		return "", fmt.Errorf("credentials: insert credential: %w", err)
	}

	if subjectType == SubjectAccount {
		if _, err := tx.ExecContext(ctx,
			`UPDATE account SET status = 'active', activated_at = COALESCE(activated_at, strftime('%Y-%m-%dT%H:%M:%fZ','now')) WHERE id = ? AND status != 'revoked'`,
			subjectID,
		); err != nil {
			return "", fmt.Errorf("credentials: activate account: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("credentials: commit put: %w", err)
	}
	return id, nil
}

// RevokeCredential immediately invalidates one credential row by ID —
// Authenticate will no longer return it.
func (s *Store) RevokeCredential(ctx context.Context, credentialID string) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE credential SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ? AND revoked_at IS NULL`,
		credentialID,
	)
	if err != nil {
		return fmt.Errorf("credentials: revoke %s: %w", credentialID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: credential %s (or already revoked)", ErrNotFound, credentialID)
	}
	return nil
}

// Authenticate decrypts and returns the active (non-revoked) credential's
// plaintext for a subject. This is the ONLY function in this package that
// returns secret material — it exists for the daemon's own connector and
// extension launch code, and must never be reachable from the admin HTTP
// API (see daemon/api's control-plane routes, which expose account/
// credential metadata but never call this).
func (s *Store) Authenticate(ctx context.Context, subjectType SubjectType, subjectID string) ([]byte, error) {
	// The active credential is the one PutCredential has neither rotated
	// out nor revoked — exactly one row per subject satisfies this at any
	// time (PutCredential rotates the prior active row in the same
	// transaction it inserts a new one). This is deliberately not an
	// ORDER BY created_at DESC LIMIT 1: two credentials written within the
	// same timestamp tick would make that ordering ambiguous.
	var ciphertext []byte
	err := s.DB.QueryRowContext(ctx,
		`SELECT ciphertext FROM credential WHERE subject_type = ? AND subject_id = ? AND rotated_at IS NULL AND revoked_at IS NULL`,
		string(subjectType), subjectID,
	).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s %s", ErrNoCredential, subjectType, subjectID)
	}
	if err != nil {
		return nil, fmt.Errorf("credentials: query credential: %w", err)
	}
	return s.decrypt(ciphertext)
}

func (s *Store) encrypt(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, fmt.Errorf("credentials: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credentials: new GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("credentials: generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func (s *Store) decrypt(blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, fmt.Errorf("credentials: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credentials: new GCM: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(blob) < nonceSize {
		return nil, fmt.Errorf("credentials: ciphertext too short")
	}
	nonce, ciphertext := blob[:nonceSize], blob[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("credentials: decrypt (wrong key, or tampered ciphertext): %w", err)
	}
	return plaintext, nil
}
