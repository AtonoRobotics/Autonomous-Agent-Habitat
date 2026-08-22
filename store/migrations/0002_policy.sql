-- Generic policy and approval seam (docs/AMH-SPECIFICATION.md §6, §2.1:
-- "generic action admission and approval-hook execution"). Domain-neutral
-- by construction: nothing here knows what an action IS, only its
-- declared digest, reversibility property, and the policy's verdict.
--
-- A PolicyDecision row is immutable once written — decidedAt/expiresAt/
-- result are never updated in place. Approving a needs_approval decision
-- mints a NEW admit decision bound to the same operation_id/action_digest
-- rather than mutating the original; this is what keeps "the result SHALL
-- bind the exact action digest, constraints, policy version, decision
-- time, and expiry" true even after approval, and what acceptance
-- invariant #6 ("policy dispatch is bound to the admitted action digest
-- and fails closed after expiry or mutation") is checking.

CREATE TABLE policy_decision (
  id TEXT PRIMARY KEY,
  operation_id TEXT NOT NULL,
  action_digest TEXT NOT NULL,
  policy_id TEXT NOT NULL,
  policy_version TEXT NOT NULL,
  result TEXT NOT NULL CHECK(result IN ('admit', 'admit_with_constraints', 'needs_approval', 'defer', 'deny')),
  constraints JSON,
  approval_request_id TEXT,
  reason_codes JSON,
  evidence_refs JSON,
  decided_at TEXT NOT NULL DEFAULT iso8601_now(),
  expires_at TEXT NOT NULL,
  -- Set exactly once, by Consume, inside the same transaction that checks
  -- it was still NULL — the atomic compare-and-set that makes "check-then-
  -- act approval is forbidden: dispatch consumes the bound decision
  -- atomically or fails closed" (§6) true rather than a hopeful comment.
  consumed_at TEXT
);
CREATE INDEX idx_policy_decision_operation ON policy_decision(operation_id, action_digest);

-- Created only for a needs_approval decision. supersedes/supersedes_new
-- are absent; resolution is recorded here, and Approve mints the new
-- admit decision in policy_decision rather than this table — this table
-- only ever tracks the human resolution, never a fresh grant.
CREATE TABLE approval_request (
  id TEXT PRIMARY KEY,
  decision_id TEXT NOT NULL REFERENCES policy_decision(id),
  status TEXT NOT NULL CHECK(status IN ('pending', 'approved', 'denied')) DEFAULT 'pending',
  resolved_by TEXT,
  resolved_at TEXT,
  reason TEXT,
  created_at TEXT NOT NULL DEFAULT iso8601_now()
);
CREATE INDEX idx_approval_request_status ON approval_request(status);
