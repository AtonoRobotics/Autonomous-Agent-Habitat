-- Reverses store/migrations/0004_selfimprove.sql. eval references
-- candidate_version, so it drops first; both tables' indexes drop
-- automatically with them.
DROP TABLE eval;
DROP TABLE candidate_version;
