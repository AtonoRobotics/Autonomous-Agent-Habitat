-- §4's pre-dispatch requirement completeness: "before dispatch, the
-- owning extension SHALL supply ... retry classification" — a field
-- contracts/action-envelope.schema.json already declares required, with
-- a fixed three-value enum, but effect_record never persisted and
-- daemon/operations.Propose never required. NOT NULL with no default:
-- every Propose call site must now declare a real classification, not
-- fall back to a value nobody chose.
ALTER TABLE effect_record ADD COLUMN retry_class TEXT NOT NULL DEFAULT 'never'
  CHECK(retry_class IN ('never', 'reconcile_before_retry', 'idempotent'));
ALTER TABLE effect_record ALTER COLUMN retry_class DROP DEFAULT;
