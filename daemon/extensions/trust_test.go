package extensions

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"testing"
)

func genKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}

func TestTrustStore_RegisterGetRevoke(t *testing.T) {
	db := testDB(t)
	ts := newTrustStore(db)
	pub, _ := genKey(t)

	k, err := ts.RegisterKey(context.Background(), "key-1", hex.EncodeToString(pub))
	if err != nil {
		t.Fatalf("RegisterKey: %v", err)
	}
	if k.RevokedAt != "" {
		t.Fatalf("expected a freshly registered key to not be revoked")
	}

	got, err := ts.Get(context.Background(), "key-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PublicKeyHex != hex.EncodeToString(pub) {
		t.Fatalf("public key mismatch")
	}

	revoked, err := ts.RevokeKey(context.Background(), "key-1")
	if err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if revoked.RevokedAt == "" {
		t.Fatalf("expected revoked_at to be set")
	}

	if _, err := ts.RevokeKey(context.Background(), "key-1"); !errors.Is(err, ErrKeyRevoked) {
		t.Fatalf("expected ErrKeyRevoked revoking an already-revoked key, got %v", err)
	}
}

func TestTrustStore_RegisterKey_RejectsDuplicateID(t *testing.T) {
	db := testDB(t)
	ts := newTrustStore(db)
	pub, _ := genKey(t)

	if _, err := ts.RegisterKey(context.Background(), "key-1", hex.EncodeToString(pub)); err != nil {
		t.Fatalf("RegisterKey: %v", err)
	}
	pub2, _ := genKey(t)
	if _, err := ts.RegisterKey(context.Background(), "key-1", hex.EncodeToString(pub2)); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("expected ErrKeyExists, got %v", err)
	}
}

func TestTrustStore_RegisterKey_RejectsInvalidPublicKey(t *testing.T) {
	db := testDB(t)
	ts := newTrustStore(db)

	if _, err := ts.RegisterKey(context.Background(), "key-1", "not-hex"); !errors.Is(err, ErrInvalidPublicKey) {
		t.Fatalf("expected ErrInvalidPublicKey for non-hex input, got %v", err)
	}
	if _, err := ts.RegisterKey(context.Background(), "key-1", "aabb"); !errors.Is(err, ErrInvalidPublicKey) {
		t.Fatalf("expected ErrInvalidPublicKey for a too-short key, got %v", err)
	}
}

func TestTrustStore_Get_UnknownID(t *testing.T) {
	db := testDB(t)
	ts := newTrustStore(db)
	if _, err := ts.Get(context.Background(), "does-not-exist"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}
}

func TestTrustStore_List_NewestFirst(t *testing.T) {
	db := testDB(t)
	ts := newTrustStore(db)
	pub1, _ := genKey(t)
	pub2, _ := genKey(t)

	if _, err := ts.RegisterKey(context.Background(), "key-a", hex.EncodeToString(pub1)); err != nil {
		t.Fatalf("RegisterKey a: %v", err)
	}
	if _, err := ts.RegisterKey(context.Background(), "key-b", hex.EncodeToString(pub2)); err != nil {
		t.Fatalf("RegisterKey b: %v", err)
	}

	list, err := ts.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(list))
	}
	if list[0].KeyID != "key-b" {
		t.Fatalf("expected newest-first order, got %+v", list)
	}
}

func TestTrustStore_Verified_FailsClosedForRevokedOrUnknownKey(t *testing.T) {
	db := testDB(t)
	ts := newTrustStore(db)
	pub, _ := genKey(t)

	if _, err := ts.verified(context.Background(), "unknown"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound for an unregistered key, got %v", err)
	}

	if _, err := ts.RegisterKey(context.Background(), "key-1", hex.EncodeToString(pub)); err != nil {
		t.Fatalf("RegisterKey: %v", err)
	}
	if _, err := ts.RevokeKey(context.Background(), "key-1"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := ts.verified(context.Background(), "key-1"); !errors.Is(err, ErrKeyRevoked) {
		t.Fatalf("expected ErrKeyRevoked for a revoked key, got %v", err)
	}
}
