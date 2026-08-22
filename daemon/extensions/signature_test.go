package extensions

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"testing"
)

// signManifest signs m's SignableDigest with priv under keyID and attaches
// the result as m.Spec.Signature, mutating m in place.
func signManifest(t *testing.T, m *Manifest, keyID string, priv ed25519.PrivateKey) {
	t.Helper()
	digest, err := m.SignableDigest()
	if err != nil {
		t.Fatalf("SignableDigest: %v", err)
	}
	sig := ed25519.Sign(priv, []byte(digest))
	m.Spec.Signature = &Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Digest:    digest,
		Value:     hex.EncodeToString(sig),
	}
}

func TestDiscover_AdmitsValidSignedManifest(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	pub, priv := genKey(t)
	if _, err := reg.Trust.RegisterKey(context.Background(), "key-1", hex.EncodeToString(pub)); err != nil {
		t.Fatalf("RegisterKey: %v", err)
	}

	m := baseManifest("amh.test/signed", "1.0.0")
	signManifest(t, &m, "key-1", priv)

	ext, err := reg.Discover(context.Background(), m)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if ext.Status != StatusDiscovered {
		t.Fatalf("expected discovered, got %s", ext.Status)
	}
}

func TestDiscover_RejectsSignature_UnknownKey(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	_, priv := genKey(t)

	m := baseManifest("amh.test/signed", "1.0.0")
	signManifest(t, &m, "never-registered", priv)

	if _, err := reg.Discover(context.Background(), m); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature for an unknown key, got %v", err)
	}
}

func TestDiscover_RejectsSignature_RevokedKey(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	pub, priv := genKey(t)
	if _, err := reg.Trust.RegisterKey(context.Background(), "key-1", hex.EncodeToString(pub)); err != nil {
		t.Fatalf("RegisterKey: %v", err)
	}
	if _, err := reg.Trust.RevokeKey(context.Background(), "key-1"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	m := baseManifest("amh.test/signed", "1.0.0")
	signManifest(t, &m, "key-1", priv)

	if _, err := reg.Discover(context.Background(), m); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature for a revoked key, got %v", err)
	}
}

func TestDiscover_RejectsSignature_TamperedContentAfterSigning(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	pub, priv := genKey(t)
	if _, err := reg.Trust.RegisterKey(context.Background(), "key-1", hex.EncodeToString(pub)); err != nil {
		t.Fatalf("RegisterKey: %v", err)
	}

	m := baseManifest("amh.test/signed", "1.0.0")
	signManifest(t, &m, "key-1", priv)
	m.Metadata.Description = "mutated after signing"

	if _, err := reg.Discover(context.Background(), m); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature for content mutated after signing, got %v", err)
	}
}

func TestDiscover_RejectsSignature_CorruptSignatureBytes(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	pub, priv := genKey(t)
	if _, err := reg.Trust.RegisterKey(context.Background(), "key-1", hex.EncodeToString(pub)); err != nil {
		t.Fatalf("RegisterKey: %v", err)
	}

	m := baseManifest("amh.test/signed", "1.0.0")
	signManifest(t, &m, "key-1", priv)
	// Flip the signature bytes without touching the digest, so it fails
	// ed25519 verification specifically, not the earlier digest check.
	raw, _ := hex.DecodeString(m.Spec.Signature.Value)
	raw[0] ^= 0xFF
	m.Spec.Signature.Value = hex.EncodeToString(raw)

	if _, err := reg.Discover(context.Background(), m); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature for a corrupted signature, got %v", err)
	}
}

func TestDiscover_RequireSignatures_RejectsUnsignedManifest(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	reg.RequireSignatures = true

	m := baseManifest("amh.test/unsigned", "1.0.0")
	if _, err := reg.Discover(context.Background(), m); !errors.Is(err, ErrSignatureRequired) {
		t.Fatalf("expected ErrSignatureRequired, got %v", err)
	}
}

func TestDiscover_RequireSignatures_AdmitsValidSignedManifest(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	reg.RequireSignatures = true
	pub, priv := genKey(t)
	if _, err := reg.Trust.RegisterKey(context.Background(), "key-1", hex.EncodeToString(pub)); err != nil {
		t.Fatalf("RegisterKey: %v", err)
	}

	m := baseManifest("amh.test/signed", "1.0.0")
	signManifest(t, &m, "key-1", priv)

	if _, err := reg.Discover(context.Background(), m); err != nil {
		t.Fatalf("Discover: %v", err)
	}
}

func TestDiscover_AdmitsUnsignedManifest_WhenSignaturesNotRequired(t *testing.T) {
	db := testDB(t)
	reg := New(db)

	m := baseManifest("amh.test/unsigned", "1.0.0")
	if _, err := reg.Discover(context.Background(), m); err != nil {
		t.Fatalf("expected an unsigned manifest to be admitted by default, got %v", err)
	}
}

func TestDiscover_RejectsIncompatibleCoreVersion(t *testing.T) {
	db := testDB(t)
	reg := New(db)

	m := baseManifest("amh.test/incompatible", "1.0.0")
	m.Spec.Compatibility.AMHCore = ">=99.0.0"

	if _, err := reg.Discover(context.Background(), m); !errors.Is(err, ErrIncompatibleCore) {
		t.Fatalf("expected ErrIncompatibleCore, got %v", err)
	}
}
