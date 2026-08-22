-- AMH control plane: the real Cordis-lifecycle extension registry, per-agent
-- computer (sandbox) provisioning, and account/credential management for
-- connectors and modules. See docs/AMH-SPECIFICATION.md v10 §1 decision 6
-- (Cordis spatiotemporal composition governs extension lifecycle) and the
-- v10 contracts/extension-manifest.schema.json this table mirrors field for
-- field.
--
-- capability/capability_effect from 0001 were DDL only — never read or
-- written by any code path. They are replaced here, not extended: their
-- shape predates the v10 extension-manifest contract and doesn't carry
-- publisher/entrypoint/isolation/compatibility/signature, which the real
-- registry (daemon/extensions) needs. Nothing references the old tables.

PRAGMA foreign_keys = OFF; -- rebuilding connector below

DROP TABLE IF EXISTS capability_effect;
DROP TABLE IF EXISTS capability;

-- ── Extension registry (Cordis lifecycle) ───────────────────────────────

CREATE TABLE extension (
  id TEXT NOT NULL,                -- namespaced id, e.g. "amh.control-plane/ui"
  version TEXT NOT NULL,           -- semver
  name TEXT NOT NULL,
  publisher TEXT NOT NULL,
  description TEXT,
  entrypoint TEXT NOT NULL,
  isolation TEXT CHECK(isolation IN ('process','wasm','container','in_process')) NOT NULL,
  provides JSON NOT NULL,          -- CapabilityRef[] {id,version,contract}
  requires JSON NOT NULL,          -- Requirement[] {capability,version_range,optional}
  actions JSON,                    -- ActionType[]
  compatibility JSON NOT NULL,     -- {amh_core, platforms[]}
  signature JSON,
  manifest_digest TEXT NOT NULL,
  status TEXT CHECK(status IN ('discovered','activating','active','quiescing','disposed','failed')) NOT NULL DEFAULT 'discovered',
  status_reason TEXT,
  runtime_handle TEXT,             -- opaque: PID, container ID, or in-process registration key
  discovered_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  activated_at TEXT,
  disposed_at TEXT,
  PRIMARY KEY (id, version)
);

-- One row per capability an active extension provides, normalized out of
-- extension.provides so dependency resolution (does some active extension
-- provide capability X in range Y?) is a query, not a JSON scan.
CREATE TABLE extension_provided_capability (
  extension_id TEXT NOT NULL,
  extension_version TEXT NOT NULL,
  capability_id TEXT NOT NULL,
  capability_version TEXT NOT NULL,
  contract TEXT,
  PRIMARY KEY (extension_id, extension_version, capability_id),
  FOREIGN KEY (extension_id, extension_version) REFERENCES extension(id, version)
);
CREATE INDEX idx_provided_capability_lookup ON extension_provided_capability(capability_id, capability_version);

-- Temporal composability: activation and disposal recorded as a
-- forward/inverse effect pair, same shape as device_effect. Disposal IS
-- the verified inverse of activation — that pairing is what makes an
-- extension a reversible, not just removable, module.
CREATE TABLE extension_effect (
  id TEXT PRIMARY KEY,
  extension_id TEXT NOT NULL,
  extension_version TEXT NOT NULL,
  effect_type TEXT NOT NULL CHECK(effect_type IN ('activate','dispose')),
  forward_payload JSON NOT NULL,
  inverse_payload JSON,
  outcome TEXT CHECK(outcome IN ('success','failed','rolled_back')) NOT NULL DEFAULT 'success',
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  FOREIGN KEY (extension_id, extension_version) REFERENCES extension(id, version)
);
CREATE INDEX idx_extension_effect_lookup ON extension_effect(extension_id, extension_version, effect_type);

-- ── Per-agent computers (sandboxes) ─────────────────────────────────────

CREATE TABLE computer (
  id TEXT PRIMARY KEY,
  agent_id TEXT REFERENCES agent(id),
  isolation TEXT CHECK(isolation IN ('process','container')) NOT NULL,
  image TEXT,                      -- container image ref, or executable path for process isolation
  status TEXT CHECK(status IN ('provisioning','ready','busy','stopped','destroyed','failed')) NOT NULL DEFAULT 'provisioning',
  runtime_handle TEXT,             -- container ID or PID
  resource_limits JSON,
  workdir TEXT,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  ready_at TEXT,
  destroyed_at TEXT,
  destroy_reason TEXT
);
CREATE INDEX idx_computer_agent ON computer(agent_id, status);

-- ── Accounts and credentials (authenticate accounts and modules) ───────

CREATE TABLE account (
  id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,          -- e.g. 'github', 'gmail', arbitrary extension-defined provider id
  display_name TEXT,
  status TEXT CHECK(status IN ('pending','active','revoked')) NOT NULL DEFAULT 'pending',
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  activated_at TEXT,
  revoked_at TEXT
);

-- Secret material never lives in plaintext at rest. subject_type/subject_id
-- point at whatever holds the credential (an account, a connector, or an
-- extension needing its own module-level secret) so one store serves all
-- three "authenticate accounts and modules" cases uniformly.
CREATE TABLE credential (
  id TEXT PRIMARY KEY,
  subject_type TEXT CHECK(subject_type IN ('account','connector','extension')) NOT NULL,
  subject_id TEXT NOT NULL,
  ciphertext BLOB NOT NULL,        -- AES-256-GCM: nonce || ciphertext || tag
  key_id TEXT NOT NULL,            -- encryption key version, for rotation without re-deriving old rows
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  rotated_at TEXT,
  revoked_at TEXT
);
CREATE INDEX idx_credential_subject ON credential(subject_type, subject_id, revoked_at);

-- ── Connectors become extension-declared, not a fixed enum ─────────────

-- 0001's connector.type CHECK enum hardcoded 8 built-in types — the exact
-- shape a genuinely pluggable connector system can't have, since an
-- extension can declare a brand new connector type the core never
-- enumerated in advance. Rebuild without the CHECK, add ownership
-- (extension_id: which extension's action types this connector serves,
-- nullable for the built-in ssh connector that predates the extension
-- system), account linkage (account_id: whose credential it authenticates
-- as), and status so a connector can be disabled without deleting its
-- config/audit history.
CREATE TABLE connector_new (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  auth TEXT NOT NULL CHECK(auth IN ('oauth2','apikey','mtls','none')) DEFAULT 'none',
  config JSON,
  extension_id TEXT,
  extension_version TEXT,
  account_id TEXT REFERENCES account(id),
  status TEXT CHECK(status IN ('active','disabled')) NOT NULL DEFAULT 'active',
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  FOREIGN KEY (extension_id, extension_version) REFERENCES extension(id, version)
);
INSERT INTO connector_new (id, type, auth, config)
  SELECT id, type, auth, config FROM connector;
DROP TABLE connector;
ALTER TABLE connector_new RENAME TO connector;

PRAGMA foreign_keys = ON;
