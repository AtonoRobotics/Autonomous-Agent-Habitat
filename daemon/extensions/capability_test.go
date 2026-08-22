package extensions

import (
	"context"
	"testing"
)

// discoverForCapabilityTest satisfies extension_capability_token's FK on
// extension(id, version) — mintCapabilityToken itself never Discovers
// (Activate, its only real caller, always runs after Discover already
// has), so these tests must set that precondition up themselves.
func discoverForCapabilityTest(t *testing.T, reg *Registry, id, version string) {
	t.Helper()
	if _, err := reg.Discover(context.Background(), baseManifest(id, version)); err != nil {
		t.Fatalf("Discover(%s, %s): %v", id, version, err)
	}
}

func TestMintAndVerifyCapabilityToken_RoundTrips(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()
	discoverForCapabilityTest(t, reg, "amh.test/widget", "1.0.0")

	token, err := mintCapabilityToken(ctx, db, "amh.test/widget", "1.0.0")
	if err != nil {
		t.Fatalf("mintCapabilityToken: %v", err)
	}
	if token == "" {
		t.Fatalf("expected a non-empty token")
	}

	id, ok, err := reg.VerifyCapabilityToken(ctx, token)
	if err != nil {
		t.Fatalf("VerifyCapabilityToken: %v", err)
	}
	if !ok || id != "amh.test/widget" {
		t.Fatalf("expected (amh.test/widget, true), got (%q, %v)", id, ok)
	}
}

func TestVerifyCapabilityToken_UnknownOrEmptyToken_IsNotOkNotError(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	id, ok, err := reg.VerifyCapabilityToken(ctx, "not-a-real-token")
	if err != nil {
		t.Fatalf("unknown token should not error: %v", err)
	}
	if ok || id != "" {
		t.Fatalf("expected (\"\", false) for an unknown token, got (%q, %v)", id, ok)
	}

	id, ok, err = reg.VerifyCapabilityToken(ctx, "")
	if err != nil {
		t.Fatalf("empty token should not error: %v", err)
	}
	if ok || id != "" {
		t.Fatalf("expected (\"\", false) for an empty token, got (%q, %v)", id, ok)
	}
}

func TestMintCapabilityToken_ReplacesThePreviousOneForTheSameIDVersion(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()
	discoverForCapabilityTest(t, reg, "amh.test/widget", "1.0.0")

	first, err := mintCapabilityToken(ctx, db, "amh.test/widget", "1.0.0")
	if err != nil {
		t.Fatalf("mint first: %v", err)
	}
	second, err := mintCapabilityToken(ctx, db, "amh.test/widget", "1.0.0")
	if err != nil {
		t.Fatalf("mint second: %v", err)
	}
	if first == second {
		t.Fatalf("expected two distinct random tokens")
	}

	if _, ok, err := reg.VerifyCapabilityToken(ctx, first); err != nil || ok {
		t.Fatalf("expected the first (replaced) token to no longer verify: ok=%v err=%v", ok, err)
	}
	id, ok, err := reg.VerifyCapabilityToken(ctx, second)
	if err != nil || !ok || id != "amh.test/widget" {
		t.Fatalf("expected the second token to verify as amh.test/widget: id=%q ok=%v err=%v", id, ok, err)
	}
}

func TestRevokeCapabilityToken_StopsItFromVerifying(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()
	discoverForCapabilityTest(t, reg, "amh.test/widget", "1.0.0")

	token, err := mintCapabilityToken(ctx, db, "amh.test/widget", "1.0.0")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := revokeCapabilityToken(ctx, db, "amh.test/widget", "1.0.0"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, ok, err := reg.VerifyCapabilityToken(ctx, token); err != nil || ok {
		t.Fatalf("expected a revoked token to no longer verify: ok=%v err=%v", ok, err)
	}
}

func TestRevokeCapabilityToken_NoTokenExists_IsANoOp(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	discoverForCapabilityTest(t, reg, "amh.test/never-activated", "1.0.0")
	if err := revokeCapabilityToken(context.Background(), db, "amh.test/never-activated", "1.0.0"); err != nil {
		t.Fatalf("expected revoking a nonexistent token to be a harmless no-op, got %v", err)
	}
}

func TestMintCapabilityToken_DistinctExtensionsGetDistinctTokens(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()
	discoverForCapabilityTest(t, reg, "amh.test/widget-a", "1.0.0")
	discoverForCapabilityTest(t, reg, "amh.test/widget-b", "1.0.0")

	tokenA, err := mintCapabilityToken(ctx, db, "amh.test/widget-a", "1.0.0")
	if err != nil {
		t.Fatalf("mint a: %v", err)
	}
	tokenB, err := mintCapabilityToken(ctx, db, "amh.test/widget-b", "1.0.0")
	if err != nil {
		t.Fatalf("mint b: %v", err)
	}

	idA, okA, errA := reg.VerifyCapabilityToken(ctx, tokenA)
	idB, okB, errB := reg.VerifyCapabilityToken(ctx, tokenB)
	if errA != nil || errB != nil || !okA || !okB {
		t.Fatalf("expected both tokens to verify: okA=%v errA=%v okB=%v errB=%v", okA, errA, okB, errB)
	}
	if idA != "amh.test/widget-a" || idB != "amh.test/widget-b" {
		t.Fatalf("expected each token to resolve to its own extension, got idA=%q idB=%q", idA, idB)
	}
}
