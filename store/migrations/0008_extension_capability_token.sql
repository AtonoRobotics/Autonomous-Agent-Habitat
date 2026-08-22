-- §15 acceptance invariant #5: "an extension cannot mutate another
-- extension's owned effects." daemon/operations' mutating HTTP routes
-- (/v1/operations/*) had no way to verify which extension a caller
-- actually was — only the two global agent/operator roles existed
-- (daemon/authn), and every process/container-isolated extension
-- inherited the same shared agent token. This table backs a narrow,
-- purpose-built capability-token mechanism scoped to exactly that gap
-- (see daemon/extensions/capability.go) rather than growing
-- daemon/authn into a general per-user identity system, which its own
-- doc comment explicitly says is out of scope.
--
-- One row per currently-activated extension instance (id, version) —
-- re-Activate replaces the row (a fresh token for a fresh process
-- instance; the old one stops verifying immediately), Dispose deletes
-- it (revocation). Only the hash is stored, never the raw token —
-- the same "never persist a secret unencrypted" posture
-- daemon/credentials already takes, though a salted hash rather than
-- reversible encryption here since this token is never read back, only
-- compared against.
CREATE TABLE extension_capability_token (
  extension_id TEXT NOT NULL,
  extension_version TEXT NOT NULL,
  token_hash TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT iso8601_now(),
  PRIMARY KEY (extension_id, extension_version),
  FOREIGN KEY (extension_id, extension_version) REFERENCES extension(id, version)
);
CREATE INDEX idx_extension_capability_token_hash ON extension_capability_token(token_hash);
