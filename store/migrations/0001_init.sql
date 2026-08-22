-- AMH core schema (PostgreSQL). Ontology and control-plane tables. See
-- docs/AMH-SPECIFICATION.md §1 (decision 4: "Postgres is authoritative
-- persistent state") and §3.3.
--
-- Memory (docs/AMH-SPECIFICATION.md §8's five projections) is NOT defined
-- here. working is a query-time projection over goal/task/run/event below
-- (agents/memory/working.py); episodic/entity/knowledge retrieval live in
-- Hindsight's own schema-isolated tables in this same Postgres cluster
-- (HINDSIGHT_API_DATABASE_SCHEMA); semantic lives in Neo4j via Graphiti.
-- procedural is the `skill` table below — genuinely AMH-owned, not a fit
-- for either external system.
--
-- Physical-device actuation, connectors, and safety cases are not part of
-- the AMH core (docs/AMH-SPECIFICATION.md §1 decision 8, §2.3, §13, §16)
-- — that domain belongs to a separate Physical AI extension, not this
-- schema.
--
-- This schema was previously SQLite (see git history prior to this
-- revision for that version and the incremental migrations that built it
-- up). It is a from-scratch Postgres baseline, not a mechanical port:
-- Postgres supports ALTER TABLE ... ADD CONSTRAINT / DROP CONSTRAINT
-- directly, so tables are defined below in their final shape rather than
-- replayed as SQLite-era rebuild-under-a-new-name patches.

-- Reproduces SQLite's strftime('%Y-%m-%dT%H:%M:%fZ','now') output exactly
-- (millisecond-precision UTC ISO-8601 with a trailing "Z") so every
-- existing string-comparison/sort over a timestamp column (valid_at <=
-- ?, ORDER BY ts, etc.) keeps working unchanged.
CREATE FUNCTION iso8601_now() RETURNS TEXT AS $$
  SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
$$ LANGUAGE SQL;

-- ── Core ontology ────────────────────────────────────────────────────────

CREATE TABLE agent (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  model TEXT,
  parent_id TEXT REFERENCES agent(id),
  state TEXT NOT NULL CHECK(state IN ('spawned','running','paused','hibernated','terminated')) DEFAULT 'spawned',
  budget_usd REAL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT iso8601_now()
);

CREATE TABLE goal (
  id TEXT PRIMARY KEY,
  text TEXT NOT NULL,
  priority INTEGER DEFAULT 0,
  owner TEXT,
  status TEXT NOT NULL CHECK(status IN ('open','active','done','failed')) DEFAULT 'open',
  created_at TEXT NOT NULL DEFAULT iso8601_now()
);

CREATE TABLE task (
  id TEXT PRIMARY KEY,
  goal_id TEXT NOT NULL REFERENCES goal(id),
  assignee TEXT REFERENCES agent(id),
  status TEXT NOT NULL DEFAULT 'open',
  approval_required INTEGER NOT NULL DEFAULT 0
);

-- procedural memory (§8): versioned skills and playbooks, distinguished
-- by `kind` — structurally identical rows, so one table, not two.
CREATE TABLE skill (
  id TEXT NOT NULL,
  version TEXT NOT NULL,
  name TEXT,
  description TEXT,
  code_ref TEXT,
  eval_score REAL,
  promoted INTEGER DEFAULT 0,
  kind TEXT NOT NULL DEFAULT 'skill' CHECK(kind IN ('skill','playbook')),
  PRIMARY KEY (id, version)
);

CREATE TABLE artifact (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES task(id),
  uri TEXT NOT NULL,
  hash TEXT
);

CREATE TABLE run (
  id TEXT PRIMARY KEY,
  task_id TEXT REFERENCES task(id),
  started TEXT NOT NULL DEFAULT iso8601_now(),
  ended TEXT,
  status TEXT NOT NULL DEFAULT 'running',
  tokens_in INTEGER DEFAULT 0,
  tokens_out INTEGER DEFAULT 0,
  cost_usd REAL DEFAULT 0
);

CREATE TABLE event (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id),
  type TEXT NOT NULL,
  ts TEXT NOT NULL DEFAULT iso8601_now(),
  payload JSON
);
CREATE INDEX idx_event_run ON event(run_id, ts);

-- ── Control plane: extension registry (Cordis lifecycle) ────────────────

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
  discovered_at TEXT NOT NULL DEFAULT iso8601_now(),
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
-- forward/inverse effect pair. Disposal IS the verified inverse of
-- activation — that pairing is what makes an extension a reversible, not
-- just removable, module.
CREATE TABLE extension_effect (
  id TEXT PRIMARY KEY,
  extension_id TEXT NOT NULL,
  extension_version TEXT NOT NULL,
  effect_type TEXT NOT NULL CHECK(effect_type IN ('activate','dispose')),
  forward_payload JSON NOT NULL,
  inverse_payload JSON,
  outcome TEXT CHECK(outcome IN ('success','failed','rolled_back')) NOT NULL DEFAULT 'success',
  created_at TEXT NOT NULL DEFAULT iso8601_now(),
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
  created_at TEXT NOT NULL DEFAULT iso8601_now(),
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
  created_at TEXT NOT NULL DEFAULT iso8601_now(),
  activated_at TEXT,
  revoked_at TEXT
);

-- Secret material never lives in plaintext at rest. subject_type/subject_id
-- point at whatever holds the credential (an account, or an extension
-- needing its own module-level secret) so one store serves both
-- "authenticate accounts and modules" cases uniformly.
CREATE TABLE credential (
  id TEXT PRIMARY KEY,
  subject_type TEXT CHECK(subject_type IN ('account','extension')) NOT NULL,
  subject_id TEXT NOT NULL,
  ciphertext BYTEA NOT NULL,       -- AES-256-GCM: nonce || ciphertext || tag
  key_id TEXT NOT NULL,            -- encryption key version, for rotation without re-deriving old rows
  created_at TEXT NOT NULL DEFAULT iso8601_now(),
  rotated_at TEXT,
  revoked_at TEXT
);
CREATE INDEX idx_credential_subject ON credential(subject_type, subject_id, revoked_at);
