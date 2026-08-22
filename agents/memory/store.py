"""Read/write accessors for four of AMH's five memory projections
(docs/AMH-SPECIFICATION.md §8): episodic, semantic, procedural, entity. The
fifth, working, is not stored here — see memory/working.py's doc comment
for why.

Deliberately thin, matching workflows/ontology.py's own convention: no
ORM, one function per operation, db_path threaded through explicitly. Built
on store/migrations/0001_init.sql's episode/fact/skill tables plus
0004_memory_projections.sql's entity_ref/entity_alias/chunk_embedding
tables and fact's supersession/contradiction/extraction-version columns.

- episodic: `episode` — an append-only log of what happened, unchanged
  after 0001_init.sql; nothing about it needed hardening for §8.
- semantic: `fact` — a bi-temporal (valid_at/invalid_at + created_at/
  expired_at) subject-predicate-object claim graph. write_claim's
  `supersedes` argument is the operational meaning of "supersession": it
  closes out the prior claim's valid_at/transaction-time window. Its
  `contradicts` argument is a pure link — §8 only requires the link be
  preserved, not that AMH auto-resolve which of two contradicting claims
  is correct; that judgment call belongs to whatever consumes
  current_claims(), not to the write path.
- procedural: `skill`, with an added `kind` column distinguishing a
  reusable "skill" from a "playbook" (§8: "versioned skills and
  playbooks") — structurally identical rows, so one table, not two.
- entity: `entity_ref`/`entity_alias`. entity_profile() is the literal
  "profile assembled from claims and evidence" §8 describes: it is not
  itself stored, only computed at read time by joining an entity's
  canonical name and aliases against every current fact that names one of
  them as subject or object.

Knowledgebase (chunk + chunk_embedding, used by memory/retrieval.py) is
also written through here: write_chunk/write_embedding.
"""

from __future__ import annotations

import hashlib
import json
import sqlite3
import uuid
from array import array
from contextlib import contextmanager
from dataclasses import dataclass
from typing import Any, Iterator

from workflows.ontology import now_iso


@contextmanager
def connect(db_path: str) -> Iterator[sqlite3.Connection]:
    conn = sqlite3.connect(db_path)
    conn.execute("PRAGMA foreign_keys = ON")
    try:
        yield conn
        conn.commit()
    finally:
        conn.close()


# ── Episodic ─────────────────────────────────────────────────────────────


@dataclass
class Episode:
    id: str
    run_id: str | None
    ts: str
    actor: str | None
    payload: dict[str, Any]
    salience: float


def write_episode(
    db_path: str, run_id: str | None, actor: str | None, payload: dict[str, Any], salience: float = 0.5
) -> str:
    episode_id = str(uuid.uuid4())
    with connect(db_path) as conn:
        conn.execute(
            "INSERT INTO episode (id, run_id, ts, actor, payload, salience) VALUES (?, ?, ?, ?, ?, ?)",
            (episode_id, run_id, now_iso(), actor, json.dumps(payload), salience),
        )
    return episode_id


def recent_episodes(db_path: str, run_id: str | None = None, limit: int = 20) -> list[Episode]:
    query = "SELECT id, run_id, ts, actor, payload, salience FROM episode"
    params: tuple[Any, ...] = ()
    if run_id is not None:
        query += " WHERE run_id = ?"
        params = (run_id,)
    query += " ORDER BY ts DESC LIMIT ?"
    params = params + (limit,)
    with connect(db_path) as conn:
        rows = conn.execute(query, params).fetchall()
    return [Episode(id=r[0], run_id=r[1], ts=r[2], actor=r[3], payload=json.loads(r[4]), salience=r[5]) for r in rows]


# ── Semantic ─────────────────────────────────────────────────────────────


@dataclass
class Claim:
    id: str
    subj: str
    pred: str
    obj: str
    valid_at: str
    invalid_at: str | None
    confidence: float
    source_episode: str | None
    extraction_version: str
    supersedes: str | None
    contradicts: str | None


def write_claim(
    db_path: str,
    subj: str,
    pred: str,
    obj: str,
    valid_at: str | None = None,
    confidence: float = 1.0,
    source_episode: str | None = None,
    extraction_version: str = "v1",
    supersedes: str | None = None,
    contradicts: str | None = None,
) -> str:
    """Writes a new claim. If `supersedes` names a prior fact id, that
    fact's valid_at/transaction-time window is closed out (both
    invalid_at and expired_at set to now) as part of the same write — this
    is the operational meaning of supersession. `contradicts` only records
    a link; it does not invalidate the fact it names (see this module's
    doc comment)."""
    fact_id = str(uuid.uuid4())
    ts = now_iso()
    with connect(db_path) as conn:
        conn.execute(
            """INSERT INTO fact
               (id, subj, pred, obj, valid_at, confidence, source_episode,
                extraction_version, supersedes, contradicts)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
            (fact_id, subj, pred, obj, valid_at or ts, confidence, source_episode, extraction_version, supersedes, contradicts),
        )
        if supersedes:
            conn.execute(
                "UPDATE fact SET invalid_at = ?, expired_at = ? WHERE id = ? AND invalid_at IS NULL",
                (ts, ts, supersedes),
            )
    return fact_id


def current_claims(
    db_path: str,
    subj: str | None = None,
    pred: str | None = None,
    obj: str | None = None,
    as_of: str | None = None,
) -> list[Claim]:
    """Returns claims valid at `as_of` (real-world/valid time; defaults to
    now) that have not been superseded (transaction time: expired_at IS
    NULL) — the standard bi-temporal "what do we currently believe" query."""
    as_of = as_of or now_iso()
    clauses = ["expired_at IS NULL", "valid_at <= ?", "(invalid_at IS NULL OR invalid_at > ?)"]
    params: list[Any] = [as_of, as_of]
    if subj is not None:
        clauses.append("subj = ?")
        params.append(subj)
    if pred is not None:
        clauses.append("pred = ?")
        params.append(pred)
    if obj is not None:
        clauses.append("obj = ?")
        params.append(obj)
    query = (
        "SELECT id, subj, pred, obj, valid_at, invalid_at, confidence, source_episode, "
        "extraction_version, supersedes, contradicts FROM fact WHERE " + " AND ".join(clauses)
    )
    with connect(db_path) as conn:
        rows = conn.execute(query, params).fetchall()
    return [
        Claim(
            id=r[0], subj=r[1], pred=r[2], obj=r[3], valid_at=r[4], invalid_at=r[5],
            confidence=r[6], source_episode=r[7], extraction_version=r[8], supersedes=r[9], contradicts=r[10],
        )
        for r in rows
    ]


# ── Procedural ───────────────────────────────────────────────────────────


@dataclass
class Skill:
    id: str
    version: str
    name: str | None
    description: str | None
    code_ref: str | None
    eval_score: float | None
    promoted: bool
    kind: str


def write_skill(
    db_path: str,
    skill_id: str,
    version: str,
    name: str | None = None,
    description: str | None = None,
    code_ref: str | None = None,
    eval_score: float | None = None,
    promoted: bool = False,
    kind: str = "skill",
) -> None:
    if kind not in ("skill", "playbook"):
        raise ValueError(f"kind must be 'skill' or 'playbook', got {kind!r}")
    with connect(db_path) as conn:
        conn.execute(
            """INSERT INTO skill (id, version, name, description, code_ref, eval_score, promoted, kind)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?)
               ON CONFLICT (id, version) DO UPDATE SET
                 name=excluded.name, description=excluded.description, code_ref=excluded.code_ref,
                 eval_score=excluded.eval_score, promoted=excluded.promoted, kind=excluded.kind""",
            (skill_id, version, name, description, code_ref, eval_score, int(promoted), kind),
        )


def promoted_skills(db_path: str, kind: str | None = None) -> list[Skill]:
    query = "SELECT id, version, name, description, code_ref, eval_score, promoted, kind FROM skill WHERE promoted = 1"
    params: tuple[Any, ...] = ()
    if kind is not None:
        query += " AND kind = ?"
        params = (kind,)
    with connect(db_path) as conn:
        rows = conn.execute(query, params).fetchall()
    return [_row_to_skill(r) for r in rows]


def get_skill(db_path: str, skill_id: str, version: str) -> Skill | None:
    with connect(db_path) as conn:
        row = conn.execute(
            "SELECT id, version, name, description, code_ref, eval_score, promoted, kind FROM skill WHERE id = ? AND version = ?",
            (skill_id, version),
        ).fetchone()
    return _row_to_skill(row) if row else None


def _row_to_skill(r: tuple[Any, ...]) -> Skill:
    return Skill(id=r[0], version=r[1], name=r[2], description=r[3], code_ref=r[4], eval_score=r[5], promoted=bool(r[6]), kind=r[7])


# ── Entity ───────────────────────────────────────────────────────────────


@dataclass
class EntityProfile:
    id: str
    kind: str
    canonical_name: str
    aliases: list[str]
    claims: list[Claim]


def upsert_entity(db_path: str, entity_id: str, kind: str, canonical_name: str, aliases: list[str] | None = None) -> None:
    with connect(db_path) as conn:
        conn.execute(
            """INSERT INTO entity_ref (id, kind, canonical_name) VALUES (?, ?, ?)
               ON CONFLICT (id) DO UPDATE SET kind=excluded.kind, canonical_name=excluded.canonical_name""",
            (entity_id, kind, canonical_name),
        )
        for alias in aliases or []:
            conn.execute("INSERT OR IGNORE INTO entity_alias (entity_id, alias) VALUES (?, ?)", (entity_id, alias))


def resolve_entity(db_path: str, name_or_alias: str) -> str | None:
    """Returns the entity_id whose canonical_name or an alias exactly
    matches (case-insensitive) — the only entity-resolution AMH performs;
    no NLP/NER model is invoked (see memory/retrieval.py's doc comment on
    why hybrid retrieval's graph leg is deliberately scoped this way)."""
    with connect(db_path) as conn:
        row = conn.execute(
            "SELECT id FROM entity_ref WHERE lower(canonical_name) = lower(?)", (name_or_alias,)
        ).fetchone()
        if row:
            return row[0]
        row = conn.execute(
            "SELECT entity_id FROM entity_alias WHERE lower(alias) = lower(?)", (name_or_alias,)
        ).fetchone()
        return row[0] if row else None


def entity_names(db_path: str, entity_id: str) -> list[str]:
    """Returns an entity's canonical name plus every alias — the full set
    of text values a fact's subj/obj might use to refer to it."""
    with connect(db_path) as conn:
        row = conn.execute("SELECT canonical_name FROM entity_ref WHERE id = ?", (entity_id,)).fetchone()
        if row is None:
            return []
        aliases = [r[0] for r in conn.execute("SELECT alias FROM entity_alias WHERE entity_id = ?", (entity_id,)).fetchall()]
    return [row[0], *aliases]


def entity_profile(db_path: str, entity_id: str, as_of: str | None = None) -> EntityProfile | None:
    with connect(db_path) as conn:
        row = conn.execute("SELECT id, kind, canonical_name FROM entity_ref WHERE id = ?", (entity_id,)).fetchone()
        if row is None:
            return None
        aliases = [r[0] for r in conn.execute("SELECT alias FROM entity_alias WHERE entity_id = ?", (entity_id,)).fetchall()]
    names = [row[2], *aliases]
    claims: list[Claim] = []
    seen_ids: set[str] = set()
    for name in names:
        for claim in current_claims(db_path, subj=name, as_of=as_of) + current_claims(db_path, obj=name, as_of=as_of):
            if claim.id not in seen_ids:
                seen_ids.add(claim.id)
                claims.append(claim)
    return EntityProfile(id=row[0], kind=row[1], canonical_name=row[2], aliases=aliases, claims=claims)


# ── Knowledgebase (chunk + vector) ──────────────────────────────────────


def write_chunk(db_path: str, chunk_id: str, doc_id: str, text: str, meta: dict[str, Any] | None = None) -> None:
    with connect(db_path) as conn:
        conn.execute(
            "INSERT INTO chunk (id, doc_id, text, meta) VALUES (?, ?, ?, ?) "
            "ON CONFLICT (id) DO UPDATE SET doc_id=excluded.doc_id, text=excluded.text, meta=excluded.meta",
            (chunk_id, doc_id, text, json.dumps(meta) if meta is not None else None),
        )


def write_embedding(
    db_path: str,
    chunk_id: str,
    text: str,
    embedding: list[float],
    embedding_identity: str,
    model_version: str,
    chunk_version: int = 1,
    acl: str = "default",
) -> None:
    """Stores one real embedding vector for a chunk, packed as a
    little-endian float32 BLOB, plus the per-vector metadata §8 SHALL
    require (embedding identity, dimension, model version, chunk version,
    source digest, ACL). `text` is the exact chunk content the embedding
    was computed over — source_digest is its sha256, so a caller can later
    detect a chunk whose text changed since it was embedded."""
    blob = array("f", embedding).tobytes()
    source_digest = hashlib.sha256(text.encode("utf-8")).hexdigest()
    with connect(db_path) as conn:
        conn.execute(
            """INSERT INTO chunk_embedding
               (chunk_id, embedding_identity, dimension, model_version, chunk_version, source_digest, acl, embedding)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?)
               ON CONFLICT (chunk_id, embedding_identity) DO UPDATE SET
                 dimension=excluded.dimension, model_version=excluded.model_version,
                 chunk_version=excluded.chunk_version, source_digest=excluded.source_digest,
                 acl=excluded.acl, embedding=excluded.embedding""",
            (chunk_id, embedding_identity, len(embedding), model_version, chunk_version, source_digest, acl, blob),
        )
