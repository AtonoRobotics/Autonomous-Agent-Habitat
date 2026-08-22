-- Reverses store/migrations/0002_policy.sql. approval_request references
-- policy_decision, so it drops first.
DROP TABLE approval_request;
DROP TABLE policy_decision;
