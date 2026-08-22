package credentials

import (
	"context"
	"crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "amh.db"), "../../store/migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	return key
}

func TestNew_RejectsWrongKeyLength(t *testing.T) {
	db := testDB(t)
	if _, err := New(db, []byte("too-short")); err == nil {
		t.Fatalf("expected an error for a non-32-byte key")
	}
}

func TestLoadKeyFromEnv_FailsClosed(t *testing.T) {
	os.Unsetenv("AMH_CREDENTIAL_KEY")
	if _, err := LoadKeyFromEnv(); err == nil {
		t.Fatalf("expected an error when AMH_CREDENTIAL_KEY is unset")
	}

	os.Setenv("AMH_CREDENTIAL_KEY", "not-valid-base64-or-right-length")
	defer os.Unsetenv("AMH_CREDENTIAL_KEY")
	if _, err := LoadKeyFromEnv(); err == nil {
		t.Fatalf("expected an error for a malformed key")
	}
}

func TestPutCredentialThenAuthenticate_RoundTrips(t *testing.T) {
	db := testDB(t)
	s, err := New(db, testKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	acct, err := s.CreateAccount(ctx, "github", "bot@example.com")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if acct.Status != AccountPending {
		t.Fatalf("expected pending, got %s", acct.Status)
	}

	secret := []byte("ghp_supersecrettoken")
	if _, err := s.PutCredential(ctx, SubjectAccount, acct.ID, secret); err != nil {
		t.Fatalf("PutCredential: %v", err)
	}

	activated, err := s.GetAccount(ctx, acct.ID)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if activated.Status != AccountActive {
		t.Fatalf("expected active after PutCredential, got %s", activated.Status)
	}

	got, err := s.Authenticate(ctx, SubjectAccount, acct.ID)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if string(got) != string(secret) {
		t.Fatalf("expected decrypted secret to round-trip, got %q", got)
	}
}

func TestCiphertext_IsNotStoredAsPlaintext(t *testing.T) {
	db := testDB(t)
	s, err := New(db, testKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	acct, _ := s.CreateAccount(ctx, "gmail", "")
	secret := []byte("extremely-secret-password-do-not-leak")
	if _, err := s.PutCredential(ctx, SubjectAccount, acct.ID, secret); err != nil {
		t.Fatalf("PutCredential: %v", err)
	}

	var ciphertext []byte
	if err := db.QueryRow(`SELECT ciphertext FROM credential WHERE subject_id = ?`, acct.ID).Scan(&ciphertext); err != nil {
		t.Fatalf("query ciphertext: %v", err)
	}
	if containsBytes(ciphertext, secret) {
		t.Fatalf("secret plaintext must never appear in the stored ciphertext blob")
	}
}

func TestAuthenticate_FailsWithWrongKey(t *testing.T) {
	db := testDB(t)
	s1, _ := New(db, testKey(t))
	ctx := context.Background()

	acct, _ := s1.CreateAccount(ctx, "github", "")
	s1.PutCredential(ctx, SubjectAccount, acct.ID, []byte("secret"))

	s2, _ := New(db, testKey(t)) // a different key entirely
	if _, err := s2.Authenticate(ctx, SubjectAccount, acct.ID); err == nil {
		t.Fatalf("expected decryption to fail with the wrong key")
	}
}

func TestPutCredential_RotatesPreviousCredential(t *testing.T) {
	db := testDB(t)
	s, _ := New(db, testKey(t))
	ctx := context.Background()

	acct, _ := s.CreateAccount(ctx, "github", "")
	firstID, err := s.PutCredential(ctx, SubjectAccount, acct.ID, []byte("v1"))
	if err != nil {
		t.Fatalf("PutCredential v1: %v", err)
	}
	if _, err := s.PutCredential(ctx, SubjectAccount, acct.ID, []byte("v2")); err != nil {
		t.Fatalf("PutCredential v2: %v", err)
	}

	got, err := s.Authenticate(ctx, SubjectAccount, acct.ID)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if string(got) != "v2" {
		t.Fatalf("expected the latest credential (v2), got %q", got)
	}

	var rotatedAt sql.NullString
	if err := db.QueryRow(`SELECT rotated_at FROM credential WHERE id = ?`, firstID).Scan(&rotatedAt); err != nil {
		t.Fatalf("query first credential: %v", err)
	}
	if !rotatedAt.Valid {
		t.Fatalf("expected the first credential to be marked rotated")
	}
}

func TestRevokeAccount_AlsoRevokesItsCredential(t *testing.T) {
	db := testDB(t)
	s, _ := New(db, testKey(t))
	ctx := context.Background()

	acct, _ := s.CreateAccount(ctx, "github", "")
	s.PutCredential(ctx, SubjectAccount, acct.ID, []byte("secret"))

	revoked, err := s.RevokeAccount(ctx, acct.ID)
	if err != nil {
		t.Fatalf("RevokeAccount: %v", err)
	}
	if revoked.Status != AccountRevoked {
		t.Fatalf("expected revoked, got %s", revoked.Status)
	}

	if _, err := s.Authenticate(ctx, SubjectAccount, acct.ID); err == nil {
		t.Fatalf("expected Authenticate to fail after account revocation")
	}
}

func TestRevokeCredential_MakesAuthenticateFail(t *testing.T) {
	db := testDB(t)
	s, _ := New(db, testKey(t))
	ctx := context.Background()

	acct, _ := s.CreateAccount(ctx, "gmail", "")
	credID, _ := s.PutCredential(ctx, SubjectAccount, acct.ID, []byte("secret"))

	if err := s.RevokeCredential(ctx, credID); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}
	if _, err := s.Authenticate(ctx, SubjectAccount, acct.ID); err == nil {
		t.Fatalf("expected Authenticate to fail for a revoked credential")
	}
}

func TestPutCredential_ServesConnectorAndExtensionSubjectsToo(t *testing.T) {
	db := testDB(t)
	s, _ := New(db, testKey(t))
	ctx := context.Background()

	if _, err := s.PutCredential(ctx, SubjectConnector, "some-connector-id", []byte("api-key")); err != nil {
		t.Fatalf("PutCredential for connector subject: %v", err)
	}
	got, err := s.Authenticate(ctx, SubjectConnector, "some-connector-id")
	if err != nil {
		t.Fatalf("Authenticate connector: %v", err)
	}
	if string(got) != "api-key" {
		t.Fatalf("got %q", got)
	}

	if _, err := s.PutCredential(ctx, "not-a-real-subject-type", "x", []byte("y")); err == nil {
		t.Fatalf("expected an error for an invalid subject_type")
	}
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
