"""End-to-end test proving memory/retrieval.py's vector leg works through
the real path: ModelClient.embed() -> real HTTP call to a real running
amh-daemon -> daemon/inference.Router.Embed -> a real (fixture) model
provider's /embeddings endpoint -> real embedding vectors -> real
brute-force cosine search in memory/retrieval.py. Every other memory test
in this suite supplies query_embedding directly; this is the one place the
whole chain is exercised together, the same relationship
test_pursue_goal.py's daemon-backed tests have to test_context.py's
pure-unit tests.
"""

from __future__ import annotations

import pytest

from conftest import deterministic_embedding
from context.llm import ModelClient
from memory import retrieval, store


def test_hybrid_retrieve_through_real_daemon_embed_call(daemon, fake_model_server, db_path):
    client = ModelClient(
        daemon_api_base_url=daemon.base_url,
        agent_token=daemon.agent_token,
        model="test-fake-model",
        embedding_model="test-fake-embedding-model",
        embedding_provider="test-fake",
    )

    store.write_chunk(db_path, "chunk-vent", "doc-1", "the greenhouse vent is stuck open")
    store.write_chunk(db_path, "chunk-irrigation", "doc-1", "irrigation schedule runs every morning")
    for chunk_id, text in (("chunk-vent", "the greenhouse vent is stuck open"), ("chunk-irrigation", "irrigation schedule runs every morning")):
        embedding = client.embed([text])[0]
        store.write_embedding(db_path, chunk_id, text, embedding, "test-fake:test-fake-embedding-model", "v1")

    # embed() went through the real daemon, but the vectors are still the
    # fixture's deterministic function of the input text — verify the real
    # HTTP round trip produced (up to float32 precision, since Go's
    # inference.EmbedResult is []float32) exactly that, not something else.
    round_tripped = client.embed(["the greenhouse vent is stuck open"])[0]
    expected = deterministic_embedding("the greenhouse vent is stuck open")
    assert round_tripped == pytest.approx(expected, abs=1e-6)

    query_embedding = client.embed(["greenhouse vent stuck open"])[0]
    results = retrieval.hybrid_retrieve(
        db_path, "vent stuck open", query_embedding=query_embedding,
        embedding_identity="test-fake:test-fake-embedding-model",
    )

    assert results[0].type == "chunk"
    assert results[0].id == "chunk-vent"
