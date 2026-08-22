-- Trusted Ed25519 signing keys for extension manifests (§14: "signed
-- extension packs and compatibility qualification"). An operator
-- registers the public keys it trusts to sign an extension manifest;
-- daemon/extensions.Discover verifies a manifest's spec.signature against
-- this store rather than trusting a keyId the manifest itself supplies.
-- Key material is immutable once registered — rotation is register-new
-- plus revoke-old, never an in-place overwrite of a keyId already in use.
CREATE TABLE trusted_signing_key (
  key_id TEXT PRIMARY KEY,
  public_key TEXT NOT NULL,   -- 32 raw Ed25519 public-key bytes, hex-encoded
  created_at TEXT NOT NULL DEFAULT iso8601_now(),
  revoked_at TEXT
);
