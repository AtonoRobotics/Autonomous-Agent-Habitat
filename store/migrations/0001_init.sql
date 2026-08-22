-- AMH core schema.
-- Ontology tables + episode log + spatial (R-tree) + vector (sqlite-vec) + FTS5.
-- See docs/AMH-SPECIFICATION.md Artifact E for the authoritative design and rationale.
--
-- NOTE: sqlite-vec is a loadable extension; the store layer loads it before
--   running this migration. rtree and fts5 are built-in SQLite modules.

PRAGMA foreign_keys = ON;

-- ── Core ontology ────────────────────────────────────────────────────────

CREATE TABLE agent (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  model TEXT,
  parent_id TEXT REFERENCES agent(id),
  state TEXT NOT NULL CHECK(state IN ('spawned','running','paused','hibernated','terminated')) DEFAULT 'spawned',
  budget_usd REAL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE goal (
  id TEXT PRIMARY KEY,
  text TEXT NOT NULL,
  priority INTEGER DEFAULT 0,
  owner TEXT,
  status TEXT NOT NULL CHECK(status IN ('open','active','done','failed')) DEFAULT 'open',
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE task (
  id TEXT PRIMARY KEY,
  goal_id TEXT NOT NULL REFERENCES goal(id),
  assignee TEXT REFERENCES agent(id),
  status TEXT NOT NULL DEFAULT 'open',
  approval_required INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE skill (
  id TEXT NOT NULL,
  version TEXT NOT NULL,
  name TEXT,
  description TEXT,
  code_ref TEXT,
  eval_score REAL,
  promoted INTEGER DEFAULT 0,
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

CREATE TABLE connector (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL CHECK(type IN ('rest','graphql','webhook','ssh','winrm','mqtt','opcua','mcp')),
  auth TEXT NOT NULL CHECK(auth IN ('oauth2','apikey','mtls','none')) DEFAULT 'none',
  config JSON
);

CREATE TABLE device (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  connector_id TEXT NOT NULL REFERENCES connector(id)
);

CREATE TABLE run (
  id TEXT PRIMARY KEY,
  task_id TEXT REFERENCES task(id),
  started TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
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
  ts TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  payload JSON
);
CREATE INDEX idx_event_run ON event(run_id, ts);

CREATE TABLE approval_gate (
  id TEXT PRIMARY KEY,
  action JSON NOT NULL,
  risk TEXT NOT NULL,
  approved_by TEXT,
  approved_at TEXT,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- ── Memory: episodic + bi-temporal semantic (Graphiti model) ───────────────

CREATE TABLE episode (
  id TEXT PRIMARY KEY,
  run_id TEXT,
  ts TEXT NOT NULL,
  actor TEXT,
  payload JSON,
  salience REAL DEFAULT 0.5
);

CREATE TABLE fact (
  id TEXT PRIMARY KEY,
  subj TEXT NOT NULL,
  pred TEXT NOT NULL,
  obj TEXT NOT NULL,
  valid_at TEXT NOT NULL,
  invalid_at TEXT,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  expired_at TEXT,
  source_episode TEXT,
  confidence REAL DEFAULT 1.0
);
CREATE INDEX idx_fact_current ON fact(subj, pred)
  WHERE invalid_at IS NULL AND expired_at IS NULL;

-- ── Knowledgebase / RAG ──────────────────────────────────────────────────

CREATE TABLE chunk (
  id TEXT PRIMARY KEY,
  doc_id TEXT NOT NULL,
  text TEXT NOT NULL,
  meta JSON
);

-- Vector index (sqlite-vec; brute-force KNN by default in V1).
-- Loaded conditionally by the store layer — dimension is env-configured
-- (VECTOR_EMBED_DIM); this migration creates it only if the extension loaded.
-- CREATE VIRTUAL TABLE chunk_vec USING vec0(embedding FLOAT[1024]);

CREATE VIRTUAL TABLE chunk_fts USING fts5(text, content='chunk', content_rowid='rowid');

-- ── Spatial memory (§7a) ─────────────────────────────────────────────────

CREATE TABLE location (
  id TEXT PRIMARY KEY,
  name TEXT,
  kind TEXT CHECK(kind IN ('point','region','hierarchy')) NOT NULL,
  coordinate_frame TEXT NOT NULL,
  x REAL, y REAL, z REAL DEFAULT 0,
  geometry JSON
);

CREATE TABLE location_containment (
  child_id TEXT NOT NULL REFERENCES location(id),
  parent_id TEXT NOT NULL REFERENCES location(id),
  PRIMARY KEY (child_id, parent_id)
);

CREATE VIRTUAL TABLE location_rtree USING rtree(
  id,
  min_x, max_x,
  min_y, max_y,
  min_z, max_z
);

-- ── Reversibility engineering (§12, §14.6) ──────────────────────────────

CREATE TABLE device_action (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL REFERENCES device(id),
  name TEXT NOT NULL,
  reversible INTEGER NOT NULL DEFAULT 0,
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
  executed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  forward_payload JSON NOT NULL,
  inverse_payload JSON,
  reversed_at TEXT,
  outcome TEXT CHECK(outcome IN ('success','fault_reversed','fault_unreversed')) NOT NULL DEFAULT 'success'
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

-- ── Capability composition (§14.6) ──────────────────────────────────────

CREATE TABLE capability (
  id TEXT NOT NULL,
  version TEXT NOT NULL,
  kind TEXT,               -- tool | connector | skill | subagent_config
  requires JSON,            -- declarative CapabilityRef[] dependencies
  mounted INTEGER NOT NULL DEFAULT 0,
  mounted_at TEXT,
  PRIMARY KEY (id, version)
);

CREATE TABLE capability_effect (
  id TEXT PRIMARY KEY,
  capability_id TEXT NOT NULL,
  capability_version TEXT NOT NULL,
  effect_type TEXT NOT NULL,
  forward_payload JSON NOT NULL,
  inverse_payload JSON,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  disposed_at TEXT,
  FOREIGN KEY (capability_id, capability_version) REFERENCES capability(id, version)
);
