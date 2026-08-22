-- Self-improvement candidate lifecycle (docs/AMH-SPECIFICATION.md §10;
-- §2.1: "independent evaluation, canary, promotion, demotion, and
-- rollback mechanics"). GENERATED -> EVALUATED -> CANARY ->
-- PROMOTED | REJECTED; PROMOTED -> DEMOTED -> ROLLED_BACK.
--
-- `ref` is deliberately opaque here — what it points at (a prompt
-- digest, a skill id+version, a module build artifact) is owned by
-- whatever produced the candidate, not this core table. This table only
-- owns the lifecycle state machine and the evidence binding, the same
-- domain-neutral discipline store/migrations/0002_policy.sql already
-- applies to action admission.
CREATE TABLE candidate_version (
  id TEXT PRIMARY KEY,
  candidate_class TEXT NOT NULL CHECK(candidate_class IN ('prompt', 'retrieval_policy', 'skill', 'module', 'core_code')),
  ref TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('generated', 'evaluated', 'canary', 'promoted', 'rejected', 'demoted', 'rolled_back')) DEFAULT 'generated',
  generated_by TEXT,
  created_at TEXT NOT NULL DEFAULT iso8601_now(),
  -- Set when entering 'canary' — Promote requires a passing Eval whose
  -- evaluated_at is AFTER this timestamp, not merely any historical pass,
  -- so canary evidence actually has to have been gathered during canary.
  canary_at TEXT,
  promoted_at TEXT,
  demoted_at TEXT,
  rolled_back_at TEXT,
  -- Set by Promote to the candidate it just demoted (same candidate_class,
  -- previously 'promoted') — Rollback uses this to restore the prior
  -- binding (acceptance invariant #11), not by inferring it after the
  -- fact from history.
  rollback_target_id TEXT REFERENCES candidate_version(id)
);
CREATE INDEX idx_candidate_version_class_status ON candidate_version(candidate_class, status);

-- Independent evidence for a candidate. `passed` is always computed by
-- daemon/selfimprove from caller-supplied raw per-case results against a
-- fixed threshold — never accepted as a caller-declared verdict (§10:
-- "No optimizer may alter its evaluator... or promotion threshold").
CREATE TABLE eval (
  id TEXT PRIMARY KEY,
  candidate_version_id TEXT NOT NULL REFERENCES candidate_version(id),
  evaluator_id TEXT NOT NULL,
  evaluator_version TEXT NOT NULL,
  metrics JSON,
  passed BOOLEAN NOT NULL,
  evaluated_at TEXT NOT NULL DEFAULT iso8601_now()
);
CREATE INDEX idx_eval_candidate ON eval(candidate_version_id, evaluated_at);
