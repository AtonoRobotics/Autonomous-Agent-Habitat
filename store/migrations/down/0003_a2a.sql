-- Reverses store/migrations/0003_a2a.sql. Narrowing goal_status_check
-- back to its pre-A2A set fails, as it should, if any row's status is
-- currently 'canceled' — that is real data this down-migration cannot
-- invent a safe replacement value for, not a bug to paper over.
ALTER TABLE goal DROP COLUMN status_message;
ALTER TABLE goal DROP CONSTRAINT goal_status_check;
ALTER TABLE goal ADD CONSTRAINT goal_status_check CHECK (status IN ('open', 'active', 'done', 'failed'));
