-- AMH core schema (PostgreSQL). Ontology, control plane, reversibility
-- engineering, and safety-case tables. See docs/AMH-SPECIFICATION.md §1
-- (decision 4: "Postgres is authoritative persistent state") and §3.3.
--
-- Memory (docs/AMH-SPECIFICATION.md §8's five projections) is NOT defined
-- here. working is a query-time projection over goal/task/run/event below
-- (agents/memory/working.py); episodic/entity/knowledge retrieval live in
-- Hindsight's own schema-isolated tables in this same Postgres cluster
-- (HINDSIGHT_API_DATABASE_SCHEMA); semantic lives in Neo4j via Graphiti.
-- procedural is the `skill` table below — genuinely AMH-owned, not a fit
-- for either external system.
--
-- This schema was previously SQLite (see git history prior to this
-- revision for that version and the incremental migrations that built it
-- up). It is a from-scratch Postgres baseline, not a mechanical port: the
-- SQLite-era table-rebuild workarounds for constraints SQLite can't ALTER
-- in place (e.g. rebuilding `connector`/`device_effect` under a new name
-- to change a CHECK) don't apply to Postgres, which supports ALTER TABLE
-- ... ADD CONSTRAINT / DROP CONSTRAINT directly — so those tables are
-- defined below in their final shape rather than replayed as patches.

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

CREATE TABLE tool (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  input_schema TEXT NOT NULL,  -- JSON Schema
  connector_id TEXT
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

CREATE TABLE approval_gate (
  id TEXT PRIMARY KEY,
  action JSON NOT NULL,
  risk TEXT NOT NULL,
  approved_by TEXT,
  approved_at TEXT,
  created_at TEXT NOT NULL DEFAULT iso8601_now(),
  -- Binds an approval ticket to the exact action digest (device_action_id
  -- + canonical params) it was approved for, and makes it single-use —
  -- an approved ticket cannot authorize a different or repeated action.
  action_digest TEXT,
  used_at TEXT
);

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
-- point at whatever holds the credential (an account, a connector, or an
-- extension needing its own module-level secret) so one store serves all
-- three "authenticate accounts and modules" cases uniformly.
CREATE TABLE credential (
  id TEXT PRIMARY KEY,
  subject_type TEXT CHECK(subject_type IN ('account','connector','extension')) NOT NULL,
  subject_id TEXT NOT NULL,
  ciphertext BYTEA NOT NULL,       -- AES-256-GCM: nonce || ciphertext || tag
  key_id TEXT NOT NULL,            -- encryption key version, for rotation without re-deriving old rows
  created_at TEXT NOT NULL DEFAULT iso8601_now(),
  rotated_at TEXT,
  revoked_at TEXT
);
CREATE INDEX idx_credential_subject ON credential(subject_type, subject_id, revoked_at);

-- ── Connectors (extension-declared, not a fixed enum) ───────────────────

CREATE TABLE connector (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  auth TEXT NOT NULL CHECK(auth IN ('oauth2','apikey','mtls','none')) DEFAULT 'none',
  config JSON,
  extension_id TEXT,
  extension_version TEXT,
  account_id TEXT REFERENCES account(id),
  status TEXT CHECK(status IN ('active','disabled')) NOT NULL DEFAULT 'active',
  created_at TEXT NOT NULL DEFAULT iso8601_now(),
  FOREIGN KEY (extension_id, extension_version) REFERENCES extension(id, version)
);

CREATE TABLE device (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  connector_id TEXT NOT NULL REFERENCES connector(id)
);

-- ── Reversibility engineering (§12, §14.6) ──────────────────────────────

CREATE TABLE device_action (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL REFERENCES device(id),
  name TEXT NOT NULL,
  reversible INTEGER NOT NULL DEFAULT 0,
  -- forward/read-state command shape, the same way inverse_template
  -- already worked: a caller supplies only named parameter values (see
  -- daemon/actuation's param validation), never shell text, so "verified
  -- reversible" and "ticket approved for this action" both mean something
  -- concrete tied to what the daemon will actually execute. All three are
  -- {"shell_template": "..."} with {{param_name}} placeholders.
  forward_template JSON,
  read_state_template JSON,
  inverse_template JSON,
  verified_at TEXT,
  autonomy_stage TEXT CHECK(autonomy_stage IN ('unverified','supervised','earned')) NOT NULL DEFAULT 'unverified',
  autonomy_policy TEXT CHECK(autonomy_policy IN ('immediate','graduated','always_gated')) NOT NULL DEFAULT 'immediate',
  min_successes_to_earn INTEGER DEFAULT 20,
  max_failure_rate REAL DEFAULT 0.0,
  graduated_at TEXT
);

CREATE TABLE device_effect (
  id TEXT PRIMARY KEY,
  device_action_id TEXT NOT NULL REFERENCES device_action(id),
  run_id TEXT,
  executed_at TEXT NOT NULL DEFAULT iso8601_now(),
  forward_payload JSON NOT NULL,
  inverse_payload JSON,
  reversed_at TEXT,
  -- 'pending' is persisted before the physical effect runs, so a
  -- journal-write failure blocks the effect instead of silently following
  -- it. 'unknown' means the shell command itself errored after the intent
  -- was durably recorded — state is uncertain, not assumed reverted.
  outcome TEXT CHECK(outcome IN ('pending','success','unknown','fault_reversed','fault_unreversed')) NOT NULL DEFAULT 'pending'
);
CREATE INDEX idx_device_effect_action ON device_effect(device_action_id, outcome);

-- ── Earned autonomy, irreversible/high-consequence track (§14.7) ───────

CREATE TABLE safety_case (
  id TEXT PRIMARY KEY,
  subject_id TEXT NOT NULL,
  subject_type TEXT CHECK(subject_type IN ('device_action','capability')) NOT NULL,
  risk_class TEXT CHECK(risk_class IN ('low','moderate','high','irreversible_high_consequence')) NOT NULL,
  guardrails JSON,
  supervised_track_record JSON,
  formal_verification JSON,
  independent_review INTEGER NOT NULL DEFAULT 0,
  approved_at TEXT,
  approved_by TEXT,
  ongoing_monitoring JSON,
  revoked_at TEXT,
  revoked_reason TEXT
);
