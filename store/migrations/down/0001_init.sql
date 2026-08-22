-- Reverses store/migrations/0001_init.sql, in dependency order: a table
-- with a foreign key drops before the table it references, and
-- iso8601_now() drops last since several columns' DEFAULT clauses depend
-- on it — Postgres refuses to drop a function still referenced by a
-- table default.
DROP TABLE credential;
DROP TABLE account;
DROP TABLE computer;
DROP TABLE extension_effect;
DROP TABLE extension_provided_capability;
DROP TABLE extension;
DROP TABLE event;
DROP TABLE run;
DROP TABLE artifact;
DROP TABLE skill;
DROP TABLE task;
DROP TABLE goal;
DROP TABLE agent;
DROP FUNCTION iso8601_now();
