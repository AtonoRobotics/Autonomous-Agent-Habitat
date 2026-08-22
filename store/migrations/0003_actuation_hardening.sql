-- Hardens the reversibility-gated autonomy path against three real gaps
-- found by PR #3's review:
--
--   1. An approved approval_gate ticket was checked by ID only — never
--      compared against the action it was actually approved for, and
--      never consumed. One approval could authorize unlimited, unrelated
--      actuations.
--   2. The daemon accepted arbitrary shell text as the "forward"/
--      "read_state" commands for any device_action, decoupled from
--      whatever reversibility verification actually covered. "Verified
--      reversible" didn't pin down what command was verified.
--   3. A device_effect journal-insert failure after a successful physical
--      actuation was indistinguishable from "never attempted" — no
--      idempotency record existed until after the effect already
--      happened.

-- device_action now owns its own forward/read-state command shape, the
-- same way it already owns inverse_template — a caller supplies only
-- named parameter values (see daemon/actuation's param validation), not
-- shell text, so "verified reversible" and "ticket approved for this
-- action" both mean something concrete tied to what the daemon will
-- actually execute. Both are the same {"shell_template": "..."} shape as
-- inverse_template, with {{param_name}} placeholders filled from the
-- caller's params map.
ALTER TABLE device_action ADD COLUMN forward_template JSON;
ALTER TABLE device_action ADD COLUMN read_state_template JSON;

-- Bind an approval ticket to the exact action digest (device_action_id +
-- canonical params) it was approved for, and make it single-use.
ALTER TABLE approval_gate ADD COLUMN action_digest TEXT;
ALTER TABLE approval_gate ADD COLUMN used_at TEXT;

-- Add 'pending' (persisted before the physical effect runs, so a
-- journal-write failure blocks the effect instead of silently following
-- it) and 'unknown' (the shell command itself errored after the intent
-- was durably recorded — state is uncertain, not assumed reverted) to
-- device_effect's outcome vocabulary. SQLite cannot ALTER a CHECK
-- constraint in place, so rebuild the table exactly as connector was
-- reworked in 0002.
PRAGMA foreign_keys = OFF;
CREATE TABLE device_effect_new (
  id TEXT PRIMARY KEY,
  device_action_id TEXT NOT NULL REFERENCES device_action(id),
  run_id TEXT,
  executed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  forward_payload JSON NOT NULL,
  inverse_payload JSON,
  reversed_at TEXT,
  outcome TEXT CHECK(outcome IN ('pending','success','unknown','fault_reversed','fault_unreversed')) NOT NULL DEFAULT 'pending'
);
INSERT INTO device_effect_new SELECT * FROM device_effect;
DROP TABLE device_effect;
ALTER TABLE device_effect_new RENAME TO device_effect;
CREATE INDEX idx_device_effect_action ON device_effect(device_action_id, outcome);
PRAGMA foreign_keys = ON;
