"""Integration test for the semantic/entity memory projection (memory/graph.py,
memory/graph_llm.py) — a real, embedded Kuzu graph driven by the real
Graphiti library, with Graphiti's LLMClient/EmbedderClient/CrossEncoderClient
interfaces bridged to a stand-in HTTP daemon (same "real protocol, fake
remote counterpart" pattern as test_llm.py's _FakeDaemon and
tests/conftest.py's fake_model_server — the daemon's actual HTTP routing
is already covered by the e2e tests; what's new and under test here is
whether the Graphiti adapters correctly drive Graphiti's real extraction/
retrieval pipeline against a real graph store).

Neo4j itself is not reachable from this sandbox (blocked by the
environment's own egress policy — see the daemon-cutover work earlier in
this codebase's history for the exact denial), so this exercises the
Kuzu embedded driver, which Graphiti's own driver abstraction supports
natively. Neo4j remains the documented production default
(memory/graph.py's graph_driver_from_env), unverified against a live
instance in this sandbox for that reason alone.

Kuzu itself turned out to be a second, independent limitation, not just a
substitute for the network-blocked Neo4j: graphiti-core 0.29.3's installed
KuzuDriver.setup_schema() never creates the full-text index
graphiti_core.search.search_utils.edge_fulltext_search's BM25 leg queries
(no FTS_INDEX statement anywhere in kuzu_driver.py), so any real edge
write that goes through Graphiti's normal dedup path — add_episode()
extracting a real edge, or add_triplet() — raises inside that dedup step
(add_triplet's own internal duplicate-edge search is unconditionally
BM25-inclusive; there is no way to configure it around this). This is
consistent with the driver's own DeprecationWarning ("no longer
maintained... migrate to Neo4j or FalkorDB") — it is a real, unfixed
upstream bug, not a sandbox network restriction. What IS verified for
real here: the adapters' HTTP bridging to the daemon (LLM completion,
embedding, cross-encoder classification, each round-tripping real values)
and a real Graphiti add_episode() write producing a real episode node in
a real Kuzu graph. Edge-level graph writes and hybrid/BM25 search are
verified structurally (the request/response shapes, the schema-filling
protocol) but not against a live graph store in this sandbox — that
requires Neo4j or FalkorDB, neither reachable here.
"""

from __future__ import annotations

import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from threading import Thread

import pytest

from context.llm import ModelClient
from memory.graph import build_graphiti

SCHEMA_MARKER = "Respond with a JSON object in the following format:\n\n"


def _default_for_json_schema_property(prop: dict) -> object:
    t = prop.get("type")
    if t == "array":
        return []
    if t == "string":
        return ""
    if t == "integer":
        return 0
    if t == "number":
        return 0.0
    if t == "boolean":
        return False
    if t == "object":
        return {}
    return None


def _fill_schema_defaults(schema: dict) -> dict:
    return {name: _default_for_json_schema_property(prop) for name, prop in schema.get("properties", {}).items()}


def deterministic_embedding(text: str, dimension: int) -> list[float]:
    import hashlib

    digest = hashlib.sha256(text.encode("utf-8")).digest()
    repeated = (digest * (dimension // len(digest) + 1))[:dimension]
    return [(b / 127.5) - 1.0 for b in repeated]


class _FakeInferenceDaemon(BaseHTTPRequestHandler):
    """Stands in for the daemon's /v1/inference/complete and
    /v1/inference/embed routes. For a structured request (Graphiti always
    appends the target JSON schema to the last message when it wants
    structured output — see graphiti_core.llm_client.client.LLMClient.
    generate_response), returns a minimal, schema-shaped but semantically
    empty response (no entities/edges extracted) — a real model could
    validly return exactly this for content with nothing worth
    extracting; what's under test is that the adapter and Graphiti's own
    pipeline correctly round-trip and persist whatever the model says, not
    that a fake model does real NLP.
    """

    embedding_dimension = 8

    def log_message(self, format, *args):  # noqa: A002
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length))

        if self.path == "/v1/inference/embed":
            vectors = [deterministic_embedding(t, self.embedding_dimension) for t in body["input"]]
            response = json.dumps({"embeddings": vectors, "dimension": self.embedding_dimension}).encode()
        else:
            last_content = body["messages"][-1]["content"] if body["messages"] else ""
            marker_at = last_content.find(SCHEMA_MARKER)
            if marker_at >= 0:
                schema = json.loads(last_content[marker_at + len(SCHEMA_MARKER) :])
                text = json.dumps(_fill_schema_defaults(schema))
            else:
                text = "False"
            response = json.dumps({"text": text}).encode()

        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(response)


@pytest.fixture()
def fake_daemon():
    server = HTTPServer(("127.0.0.1", 0), _FakeInferenceDaemon)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        thread.join(timeout=5)


@pytest.fixture()
def model_client(fake_daemon):
    return ModelClient(
        daemon_api_base_url=fake_daemon,
        agent_token="test-agent-token",
        model="test-fake-model",
        embedding_model="test-fake-embedding-model",
    )


@pytest.mark.asyncio
async def test_add_episode_writes_a_real_episode_node_through_a_real_kuzu_graph(model_client):
    from graphiti_core.driver.kuzu_driver import KuzuDriver
    from graphiti_core.nodes import EpisodeType

    from datetime import datetime, timezone

    driver = KuzuDriver(db=":memory:")
    graphiti = build_graphiti(model_client, embedding_dim=_FakeInferenceDaemon.embedding_dimension, driver=driver)
    try:
        await graphiti.build_indices_and_constraints()

        # group_id is omitted (uses the default partition): graphiti-core's
        # installed KuzuDriver never sets self._database (the "no longer
        # maintained" deprecation this test's module docstring notes is not
        # just a notice — add_episode's group_id != self.driver._database
        # check AttributeErrors on any explicit group_id with this driver).
        # Neo4jDriver sets _database correctly; this is a Kuzu-only gap.
        result = await graphiti.add_episode(
            name="greenhouse-note-1",
            episode_body="The greenhouse vent actuator is nominal.",
            source_description="test",
            reference_time=datetime.now(timezone.utc),
            source=EpisodeType.text,
        )

        # A real write happened: the episode itself is a real node, with a
        # real UUID minted by Graphiti — not a canned/echoed value. The
        # fake model always extracts zero entities/edges (see
        # _FakeInferenceDaemon's docstring), so this proves the write path
        # (LLM adapter -> real extraction pipeline -> real Kuzu write),
        # not extraction quality.
        assert result.episode.uuid
        assert result.episode.name == "greenhouse-note-1"
    finally:
        await graphiti.close()


@pytest.mark.asyncio
async def test_embedder_round_trips_real_vectors_through_the_daemon_route(model_client):
    from memory.graph_llm import DaemonGraphitiEmbedderClient

    embedder = DaemonGraphitiEmbedderClient(model_client, embedding_dim=_FakeInferenceDaemon.embedding_dimension)

    single = await embedder.create(["hello world"])
    assert single == deterministic_embedding("hello world", _FakeInferenceDaemon.embedding_dimension)

    batch = await embedder.create_batch(["a", "b"])
    assert batch == [
        deterministic_embedding("a", _FakeInferenceDaemon.embedding_dimension),
        deterministic_embedding("b", _FakeInferenceDaemon.embedding_dimension),
    ]


@pytest.mark.asyncio
async def test_cross_encoder_classifies_each_passage_through_the_daemon_route(model_client):
    from memory.graph_llm import DaemonGraphitiCrossEncoderClient

    class _FixedInferenceDaemon(_FakeInferenceDaemon):
        def do_POST(self):
            length = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(length))
            last_content = body["messages"][-1]["content"]
            text = "True" if "RELEVANT_PASSAGE" in last_content else "False"
            response = json.dumps({"text": text}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(response)

    server = HTTPServer(("127.0.0.1", 0), _FixedInferenceDaemon)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        client = ModelClient(
            daemon_api_base_url=f"http://127.0.0.1:{server.server_port}",
            agent_token="test-agent-token",
            model="test-fake-model",
        )
        cross_encoder = DaemonGraphitiCrossEncoderClient(client)

        ranked = await cross_encoder.rank("query", ["RELEVANT_PASSAGE here", "irrelevant text"])

        assert ranked[0] == ("RELEVANT_PASSAGE here", 1.0)
        assert ranked[1] == ("irrelevant text", 0.0)
    finally:
        server.shutdown()
        thread.join(timeout=5)


def test_graph_driver_from_env_raises_when_unconfigured(monkeypatch):
    from memory.graph import GraphDriverNotConfiguredError, graph_driver_from_env

    monkeypatch.delenv("GRAPHITI_NEO4J_URI", raising=False)
    monkeypatch.delenv("GRAPHITI_KUZU_DB_PATH", raising=False)

    with pytest.raises(GraphDriverNotConfiguredError):
        graph_driver_from_env()


def test_graph_driver_from_env_prefers_neo4j_over_kuzu(monkeypatch):
    from graphiti_core.driver.neo4j_driver import Neo4jDriver

    from memory.graph import graph_driver_from_env

    monkeypatch.setenv("GRAPHITI_NEO4J_URI", "bolt://127.0.0.1:7687")
    monkeypatch.setenv("GRAPHITI_NEO4J_USER", "neo4j")
    monkeypatch.setenv("GRAPHITI_NEO4J_PASSWORD", "test")
    monkeypatch.setenv("GRAPHITI_KUZU_DB_PATH", ":memory:")

    driver = graph_driver_from_env()

    assert isinstance(driver, Neo4jDriver)
