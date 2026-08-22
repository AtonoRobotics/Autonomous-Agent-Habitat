-- Generic external-effect lifecycle (docs/AMH-SPECIFICATION.md §4, §11;
-- acceptance invariant #2). Every dispatch of an external effect (MCP
-- server, API, other process) is meant to be tracked through this state
-- machine so an interrupted dispatch is durably recorded as
-- 'outcome_unknown' and surfaced for reconciliation, rather than the core
-- inferring success, failure, or a recovery action on its own behalf.
--
-- Mirrors contracts/effect-record.schema.json's identity/state fields.
-- decision_id and forward_digest carry the daemon/policy.PolicyDecision
-- that admitted this effect — see daemon/operations for how Propose and
-- MarkDispatchPending compose with that seam rather than duplicating
-- admission logic. This table tracks exactly one attempt per
-- operation_id — see daemon/operations's doc comment for the
-- retry-orchestration scope this deliberately does not cover yet.
CREATE TABLE effect_record (
  effect_id TEXT PRIMARY KEY,
  operation_id TEXT NOT NULL,
  owner_extension_id TEXT NOT NULL,
  effect_type TEXT NOT NULL,
  decision_id TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN (
    'proposed', 'admitted', 'rejected', 'needs_approval', 'dispatch_pending',
    'dispatched', 'observed', 'outcome_unknown', 'confirmed', 'reconciled',
    'compensated', 'failed'
  )),
  forward_digest TEXT NOT NULL,
  external_command_id TEXT,
  observation_ref TEXT,
  error_code TEXT,
  error_retryable BOOLEAN,
  error_message TEXT,
  attempt INTEGER NOT NULL DEFAULT 0,
  sequence INTEGER NOT NULL DEFAULT 0,
  row_version INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT iso8601_now(),
  updated_at TEXT NOT NULL DEFAULT iso8601_now()
);
CREATE INDEX idx_effect_record_operation ON effect_record(operation_id);
CREATE INDEX idx_effect_record_state ON effect_record(state);
