"""Tests for memory/retrieval.py's hybrid (lexical + vector + graph)
retrieval, each leg exercised individually plus the RRF fusion. Pure
SQLite-level tests — query_embedding is supplied directly here rather than
computed via ModelClient.embed(), matching hybrid_retrieve's own contract
(it takes an embedding, not a model client). The daemon-backed embed() call
itself is covered by test_llm.py and the real interop test in
test_memory_e2e.py.
"""

from __future__ import annotations

from memory import retrieval, store


def _seed_chunks(db_path):
    store.write_chunk(db_path, "chunk-vent", "doc-1", "the greenhouse vent is stuck open and needs manual override")
    store.write_chunk(db_path, "chunk-irrigation", "doc-1", "irrigation schedule runs every morning at 6am")
    store.write_embedding(db_path, "chunk-vent", "the greenhouse vent is stuck open and needs manual override", [1.0, 0.0, 0.0], "test-embed", "v1")
    store.write_embedding(db_path, "chunk-irrigation", "irrigation schedule runs every morning at 6am", [0.0, 1.0, 0.0], "test-embed", "v1")


def test_lexical_search_ranks_by_bm25(db_path):
    _seed_chunks(db_path)
    hits = retrieval.lexical_search(db_path, "vent")
    assert [h.chunk_id for h in hits] == ["chunk-vent"]


def test_lexical_search_finds_nothing_for_unmatched_query(db_path):
    _seed_chunks(db_path)
    assert retrieval.lexical_search(db_path, "spaceship") == []


def test_lexical_search_reflects_chunk_updates_via_fts_triggers(db_path):
    store.write_chunk(db_path, "chunk-1", "doc-1", "original text about vents")
    assert [h.chunk_id for h in retrieval.lexical_search(db_path, "vents")] == ["chunk-1"]
    store.write_chunk(db_path, "chunk-1", "doc-1", "completely different content")
    assert retrieval.lexical_search(db_path, "vents") == []
    assert [h.chunk_id for h in retrieval.lexical_search(db_path, "different")] == ["chunk-1"]


def test_vector_search_ranks_by_cosine_similarity(db_path):
    _seed_chunks(db_path)
    hits = retrieval.vector_search(db_path, [0.9, 0.1, 0.0], "test-embed")
    assert [h.chunk_id for h in hits] == ["chunk-vent", "chunk-irrigation"]
    assert hits[0].similarity > hits[1].similarity


def test_vector_search_skips_rows_of_a_different_embedding_identity(db_path):
    _seed_chunks(db_path)
    store.write_chunk(db_path, "chunk-other-model", "doc-2", "unrelated chunk")
    store.write_embedding(db_path, "chunk-other-model", "unrelated chunk", [0.0, 0.0, 1.0, 0.0], "other-embed", "v1")
    hits = retrieval.vector_search(db_path, [1.0, 0.0, 0.0], "test-embed")
    assert {h.chunk_id for h in hits} == {"chunk-vent", "chunk-irrigation"}


def test_graph_search_bfs_finds_direct_and_multi_hop_facts(db_path):
    store.write_claim(db_path, subj="vent-1", pred="located_in", obj="zone-a")
    store.write_claim(db_path, subj="zone-a", pred="controlled_by", obj="controller-1")
    store.write_claim(db_path, subj="unrelated", pred="has_state", obj="off")

    one_hop = retrieval.graph_search(db_path, ["vent-1"], hops=1)
    assert [(h.subj, h.pred, h.obj, h.hops) for h in one_hop] == [("vent-1", "located_in", "zone-a", 1)]

    two_hop = retrieval.graph_search(db_path, ["vent-1"], hops=2)
    facts = {(h.subj, h.pred, h.obj) for h in two_hop}
    assert facts == {("vent-1", "located_in", "zone-a"), ("zone-a", "controlled_by", "controller-1")}
    assert next(h for h in two_hop if h.obj == "controller-1").hops == 2


def test_graph_search_with_no_seeds_returns_nothing(db_path):
    store.write_claim(db_path, subj="vent-1", pred="located_in", obj="zone-a")
    assert retrieval.graph_search(db_path, []) == []


def test_hybrid_retrieve_fuses_all_three_legs(db_path):
    _seed_chunks(db_path)
    store.write_claim(db_path, subj="vent-1", pred="has_state", obj="stuck_open")

    results = retrieval.hybrid_retrieve(
        db_path, "vent stuck open", query_embedding=[0.9, 0.1, 0.0], embedding_identity="test-embed",
        seed_entities=["vent-1"],
    )

    kinds_and_ids = {(r.type, r.id) for r in results}
    assert ("chunk", "chunk-vent") in kinds_and_ids
    assert any(r.type == "fact" and r.content["obj"] == "stuck_open" for r in results)
    # The chunk hit that wins both the lexical AND vector legs must outrank
    # a chunk that only wins one leg.
    scores_by_key = {(r.type, r.id): r.score for r in results}
    assert scores_by_key[("chunk", "chunk-vent")] > scores_by_key[("chunk", "chunk-irrigation")]


def test_hybrid_retrieve_resolves_seed_entities_from_query_when_not_given(db_path):
    _seed_chunks(db_path)
    store.upsert_entity(db_path, "ent-1", kind="device", canonical_name="vent-1", aliases=["the vent"])
    store.write_claim(db_path, subj="vent-1", pred="has_state", obj="stuck_open")

    results = retrieval.hybrid_retrieve(db_path, "vent-1", query_embedding=[0.9, 0.1, 0.0], embedding_identity="test-embed")

    assert any(r.type == "fact" and r.content["obj"] == "stuck_open" for r in results)


def test_hybrid_retrieve_respects_limit(db_path):
    _seed_chunks(db_path)
    results = retrieval.hybrid_retrieve(db_path, "vent", query_embedding=[0.9, 0.1, 0.0], embedding_identity="test-embed", limit=1)
    assert len(results) == 1
