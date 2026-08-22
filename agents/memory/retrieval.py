"""Hybrid knowledge retrieval (docs/AMH-SPECIFICATION.md §8: "Knowledge
retrieval SHALL combine lexical, vector, and graph retrieval behind
ports"). Three independent retrieval legs, fused by Reciprocal Rank Fusion
(Cormack, Clarke & Buettcher 2009) — a standard, published rank-fusion
technique chosen specifically because it needs only each leg's ranking,
not comparable score scales, which lexical bm25, cosine similarity, and
graph hop-distance obviously are not:

- lexical: chunk_fts (FTS5, external-content, §8: "SQLite FTS5 ... V1
  implementation") ranked by bm25.
- vector: chunk_embedding, ranked by brute-force cosine similarity in pure
  Python (§8: "sqlite-vec ... V1 implementation", but see
  0004_memory_projections.sql's chunk_embedding doc comment for why this
  is a plain table + Python loop rather than a native `vec0` extension —
  the Go daemon's SQLite driver cannot load one, and retrieval already
  lives entirely in this Python layer).
- graph: BFS over the semantic projection's fact table (memory/store.py),
  seeded from entities named in the query. Entity recognition here is
  deliberately just exact/case-insensitive alias lookup
  (memory.store.resolve_entity) — not an NER model. Building or wiring in
  a real named-entity recognizer is a materially different, separately-
  scoped piece of work; a fake/heuristic one would violate this
  codebase's "no fake logic" rule more than simply doing less. A caller
  that already knows the entities in play may pass seed_entities directly
  and skip resolution entirely.

hybrid_retrieve() is the composed port: given a query and (optionally)
seed entities, it runs all three legs and returns one fused ranking.
"""

from __future__ import annotations

import math
from array import array
from dataclasses import dataclass, field
from typing import Any

from memory.store import connect, entity_names, resolve_entity

RRF_K_DEFAULT = 60


@dataclass
class LexicalHit:
    chunk_id: str
    doc_id: str
    text: str
    bm25: float


@dataclass
class VectorHit:
    chunk_id: str
    similarity: float


@dataclass
class GraphHit:
    fact_id: str
    subj: str
    pred: str
    obj: str
    confidence: float
    hops: int


@dataclass
class RetrievalResult:
    type: str  # "chunk" | "fact"
    id: str
    score: float
    content: dict[str, Any] = field(default_factory=dict)


def _fts5_and_query(text: str) -> str | None:
    """Builds an FTS5 MATCH expression requiring every whitespace-split
    token of `text` to appear (in any order/position), each token quoted
    as its own phrase — raw text can otherwise contain FTS5 query-syntax
    operators (-, :, *, AND/OR/NOT, ...) that either change the query's
    meaning unexpectedly or raise a syntax error outright, and callers
    here mean "search for this text", not "parse this as an FTS5 query".
    Returns None for a query with no tokens (an empty MATCH is itself a
    syntax error)."""
    tokens = text.split()
    if not tokens:
        return None
    return " AND ".join('"' + t.replace('"', '""') + '"' for t in tokens)


def lexical_search(db_path: str, query: str, limit: int = 20) -> list[LexicalHit]:
    """Ranks chunks by FTS5 bm25 (ascending — SQLite's bm25() returns
    unnormalized scores that decrease as relevance improves)."""
    fts_query = _fts5_and_query(query)
    if fts_query is None:
        return []
    with connect(db_path) as conn:
        rows = conn.execute(
            """SELECT chunk.id, chunk.doc_id, chunk.text, bm25(chunk_fts)
               FROM chunk_fts JOIN chunk ON chunk.rowid = chunk_fts.rowid
               WHERE chunk_fts MATCH ?
               ORDER BY bm25(chunk_fts)
               LIMIT ?""",
            (fts_query, limit),
        ).fetchall()
    return [LexicalHit(chunk_id=r[0], doc_id=r[1], text=r[2], bm25=r[3]) for r in rows]


def _cosine_similarity(a: list[float], b: list[float]) -> float:
    dot = sum(x * y for x, y in zip(a, b))
    norm_a = math.sqrt(sum(x * x for x in a))
    norm_b = math.sqrt(sum(x * x for x in b))
    if norm_a == 0.0 or norm_b == 0.0:
        return 0.0
    return dot / (norm_a * norm_b)


def vector_search(db_path: str, query_embedding: list[float], embedding_identity: str, limit: int = 20) -> list[VectorHit]:
    """Brute-force cosine KNN over chunk_embedding (§8, V1). Every
    embedding is unpacked from its little-endian float32 BLOB; a row whose
    stored dimension disagrees with query_embedding's is skipped rather
    than compared against — a real dimension mismatch, not something to
    silently coerce."""
    dimension = len(query_embedding)
    with connect(db_path) as conn:
        rows = conn.execute(
            "SELECT chunk_id, dimension, embedding FROM chunk_embedding WHERE embedding_identity = ?",
            (embedding_identity,),
        ).fetchall()
    hits: list[VectorHit] = []
    for chunk_id, stored_dimension, blob in rows:
        if stored_dimension != dimension:
            continue
        vector = array("f")
        vector.frombytes(blob)
        hits.append(VectorHit(chunk_id=chunk_id, similarity=_cosine_similarity(query_embedding, vector.tolist())))
    hits.sort(key=lambda h: h.similarity, reverse=True)
    return hits[:limit]


def graph_search(db_path: str, seed_entities: list[str], hops: int = 2, as_of: str | None = None, limit: int = 20) -> list[GraphHit]:
    """BFS over the current (bi-temporally valid) fact graph, starting
    from seed_entities and expanding through each hop's subject/object
    text. A node is visited at most once, so hop distance is the shortest
    path from any seed."""
    from memory.store import current_claims

    visited: set[str] = set(seed_entities)
    frontier: set[str] = set(seed_entities)
    found: dict[str, GraphHit] = {}
    for hop in range(1, hops + 1):
        if not frontier or len(found) >= limit:
            break
        next_frontier: set[str] = set()
        for node in frontier:
            for claim in current_claims(db_path, subj=node, as_of=as_of) + current_claims(db_path, obj=node, as_of=as_of):
                if claim.id not in found:
                    found[claim.id] = GraphHit(fact_id=claim.id, subj=claim.subj, pred=claim.pred, obj=claim.obj, confidence=claim.confidence, hops=hop)
                for other in (claim.subj, claim.obj):
                    if other not in visited:
                        visited.add(other)
                        next_frontier.add(other)
        frontier = next_frontier
    hits = list(found.values())
    hits.sort(key=lambda h: (h.hops, -h.confidence))
    return hits[:limit]


def _reciprocal_rank_fusion(ranked_key_lists: list[list[str]], k: int = RRF_K_DEFAULT) -> dict[str, float]:
    scores: dict[str, float] = {}
    for ranked_keys in ranked_key_lists:
        for rank, key in enumerate(ranked_keys, start=1):
            scores[key] = scores.get(key, 0.0) + 1.0 / (k + rank)
    return scores


def hybrid_retrieve(
    db_path: str,
    query: str,
    query_embedding: list[float],
    embedding_identity: str,
    seed_entities: list[str] | None = None,
    hops: int = 2,
    leg_limit: int = 20,
    limit: int = 10,
    k: int = RRF_K_DEFAULT,
) -> list[RetrievalResult]:
    """The composed hybrid-retrieval port: runs all three legs and returns
    one fused ranking. Callers own computing query_embedding (via
    context.llm.ModelClient.embed) — this module has no model-provider
    dependency of its own, matching every other daemon-fronted seam in
    this codebase."""
    lexical_hits = lexical_search(db_path, query, limit=leg_limit)
    vector_hits = vector_search(db_path, query_embedding, embedding_identity, limit=leg_limit)

    resolved_seeds = list(seed_entities) if seed_entities else []
    if not resolved_seeds:
        entity_id = resolve_entity(db_path, query.strip())
        if entity_id is not None:
            resolved_seeds = entity_names(db_path, entity_id)
    graph_hits = graph_search(db_path, resolved_seeds, hops=hops, limit=leg_limit) if resolved_seeds else []

    lexical_keys = [f"chunk:{h.chunk_id}" for h in lexical_hits]
    vector_keys = [f"chunk:{h.chunk_id}" for h in vector_hits]
    graph_keys = [f"fact:{h.fact_id}" for h in graph_hits]
    fused_scores = _reciprocal_rank_fusion([lexical_keys, vector_keys, graph_keys], k=k)

    chunk_by_id = {h.chunk_id: h for h in lexical_hits}
    for h in vector_hits:
        chunk_by_id.setdefault(h.chunk_id, h)
    fact_by_id = {h.fact_id: h for h in graph_hits}

    results: list[RetrievalResult] = []
    for key, score in fused_scores.items():
        kind, _, item_id = key.partition(":")
        if kind == "chunk":
            hit = chunk_by_id.get(item_id)
            content = {"text": hit.text, "doc_id": hit.doc_id} if isinstance(hit, LexicalHit) else {}
            results.append(RetrievalResult(type="chunk", id=item_id, score=score, content=content))
        else:
            fact = fact_by_id[item_id]
            results.append(RetrievalResult(type="fact", id=item_id, score=score, content={"subj": fact.subj, "pred": fact.pred, "obj": fact.obj, "confidence": fact.confidence}))

    results.sort(key=lambda r: r.score, reverse=True)
    return results[:limit]
