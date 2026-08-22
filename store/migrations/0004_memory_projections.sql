-- Hardens the five memory projections named in docs/AMH-SPECIFICATION.md §8
-- (working, episodic, semantic, procedural, entity) and their hybrid
-- (lexical + vector + graph) retrieval. See agents/memory for the read/write
-- API built on this schema.
--
-- working: NOT a table here — it's a query-time projection over the
--   existing goal/task/run/event tables (agents/memory/working.py). §8 calls
--   it "active task/context projection", i.e. derived, not separately stored.
-- episodic: the existing `episode` table (0001_init.sql) already fits;
--   unchanged here.
-- semantic: the existing `fact` table is already bi-temporal (valid_at/
--   invalid_at + created_at/expired_at) but §8 additionally requires SHALL
--   "contradiction/supersession links, and extraction version" — added below.
-- procedural: the existing `skill` table already models versioned skills;
--   §8 says "versioned skills and playbooks" — added a `kind` discriminator
--   rather than a second table, since playbooks are structurally identical
--   (id, version, name, description, code_ref, eval_score, promoted).
-- entity: no existing table — new entity_ref/entity_alias below. A profile
--   is assembled at query time (entity_ref + aliases + every current fact
--   whose subj/obj matches the canonical name or an alias), not stored
--   redundantly — matches §8's "profiles assembled from claims and evidence".

PRAGMA foreign_keys = ON;

-- ── Semantic: contradiction/supersession + extraction version (§8 SHALL) ──

ALTER TABLE fact ADD COLUMN supersedes TEXT REFERENCES fact(id);
ALTER TABLE fact ADD COLUMN contradicts TEXT REFERENCES fact(id);
ALTER TABLE fact ADD COLUMN extraction_version TEXT NOT NULL DEFAULT 'v1';

-- ── Procedural: skills and playbooks ────────────────────────────────────

ALTER TABLE skill ADD COLUMN kind TEXT NOT NULL DEFAULT 'skill' CHECK(kind IN ('skill','playbook'));

-- ── Entity: profiles assembled from claims and evidence ─────────────────

CREATE TABLE entity_ref (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  canonical_name TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE entity_alias (
  entity_id TEXT NOT NULL REFERENCES entity_ref(id),
  alias TEXT NOT NULL,
  PRIMARY KEY (entity_id, alias)
);
CREATE INDEX idx_entity_alias_alias ON entity_alias(alias);

-- ── Knowledgebase vector metadata (§8 SHALL) ─────────────────────────────
-- Every vector carries embedding identity, dimension, model version, chunk
-- version, source digest, and ACL/visibility metadata, per §8. Stored as a
-- plain table with the embedding packed as a little-endian float32 BLOB and
-- searched by brute-force cosine similarity in agents/memory/retrieval.py
-- (".env.example: VECTOR_DB=sqlite-vec ... Brute-force KNN by default (V1)")
-- rather than a native `vec0` virtual table: daemon/store uses
-- modernc.org/sqlite, a pure-Go SQLite implementation with no CGO bridge to
-- link a native sqlite-vec extension against, and retrieval is driven
-- entirely from the Python agent layer, not the Go daemon, so there is no
-- reason to require one.

CREATE TABLE chunk_embedding (
  chunk_id TEXT NOT NULL REFERENCES chunk(id),
  embedding_identity TEXT NOT NULL,
  dimension INTEGER NOT NULL,
  model_version TEXT NOT NULL,
  chunk_version INTEGER NOT NULL DEFAULT 1,
  source_digest TEXT NOT NULL,
  acl TEXT NOT NULL DEFAULT 'default',
  embedding BLOB NOT NULL,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  PRIMARY KEY (chunk_id, embedding_identity)
);

-- ── chunk_fts sync triggers ──────────────────────────────────────────────
-- chunk_fts (0001_init.sql) is an external-content FTS5 table (content=
-- 'chunk'), which SQLite never populates automatically — without these
-- triggers it stays permanently empty and lexical retrieval silently
-- returns nothing. Standard SQLite FTS5 external-content trigger pattern.

CREATE TRIGGER chunk_ai AFTER INSERT ON chunk BEGIN
  INSERT INTO chunk_fts(rowid, text) VALUES (new.rowid, new.text);
END;

CREATE TRIGGER chunk_ad AFTER DELETE ON chunk BEGIN
  INSERT INTO chunk_fts(chunk_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
END;

CREATE TRIGGER chunk_au AFTER UPDATE ON chunk BEGIN
  INSERT INTO chunk_fts(chunk_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
  INSERT INTO chunk_fts(rowid, text) VALUES (new.rowid, new.text);
END;
