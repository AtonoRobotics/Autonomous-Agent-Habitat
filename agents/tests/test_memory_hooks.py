"""Tests for workflows/memory_hooks.py — recall_context/retain_outcome,
the first real consumers of episodic (Hindsight) and semantic/entity
(Graphiti/Neo4j) memory in this codebase.

Every test here sets ADAPTER_MODEL/routes ModelClient at a stand-in HTTP
daemon (same "real protocol, fake remote counterpart" pattern as
test_llm.py/test_memory_graph.py) — real requests reach real code, only
the remote counterpart is a fixture.

Graphiti's success path (a real search() call returning real facts) is
not verified here: graphiti-core's installed KuzuDriver cannot execute
search() at all — even against an empty graph — because it never creates
the full-text index search() unconditionally queries (a real, structural
bug documented in tests/test_memory_graph.py and memory/graph.py, not
specific to a populated graph). Neo4j does not have this gap. What IS
verified for the Graphiti leg: driver selection (env vars reaching
graph_driver_from_env correctly) and that a real failure post-configuration
(this exact Kuzu bug) propagates rather than being swallowed as if
Graphiti were simply unconfigured — the specific behavior recall_context/
retain_outcome must get right to avoid masking a real outage as "memory
just isn't enabled here."
"""

from __future__ import annotations

import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from threading import Thread

import pytest

from workflows.memory_hooks import recall_context, retain_outcome


class _FakeInferenceDaemon(BaseHTTPRequestHandler):
    """Stands in for the daemon's /v1/inference/* routes. Completions
    always return "{}" (a real model's valid answer for "nothing to
    extract"); embeddings return a fixed-dimension deterministic vector."""

    def log_message(self, format, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length))
        if self.path == "/v1/inference/embed":
            response = json.dumps({"embeddings": [[0.1] * 8 for _ in body["input"]], "dimension": 8}).encode()
        else:
            response = json.dumps({"text": "{}"}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(response)


@pytest.fixture()
def fake_daemon(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "test-fake-model")
    monkeypatch.setenv("ADAPTER_EMBEDDING_MODEL", "test-fake-embedding-model")
    server = HTTPServer(("127.0.0.1", 0), _FakeInferenceDaemon)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        thread.join(timeout=5)


def _clear_memory_env(monkeypatch):
    for var in ("HINDSIGHT_BASE_URL", "HINDSIGHT_API_KEY", "GRAPHITI_NEO4J_URI", "GRAPHITI_KUZU_DB_PATH"):
        monkeypatch.delenv(var, raising=False)


def test_recall_context_returns_empty_when_nothing_configured(monkeypatch, fake_daemon):
    _clear_memory_env(monkeypatch)
    result = recall_context("a goal about greenhouses", fake_daemon, "tok")
    assert result == ""


def test_retain_outcome_is_a_silent_no_op_when_nothing_configured(monkeypatch, fake_daemon):
    _clear_memory_env(monkeypatch)
    # Must not raise — retain_outcome(...) returning normally IS the assertion.
    retain_outcome("a goal", "a summary", fake_daemon, "tok")


class _FakeHindsightServer(BaseHTTPRequestHandler):
    captured_bodies: list[dict] = []

    def log_message(self, format, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length)) if length else {}
        type(self).captured_bodies.append({"path": self.path, "body": body})

        if self.path.endswith("/recall"):
            response = json.dumps({"results": [{"id": "u1", "text": "the greenhouse vent was opened last week"}]}).encode()
        else:
            response = json.dumps({"success": True, "bank_id": body.get("bank_id", "b"), "items_count": 1, "async": False}).encode()

        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(response)


@pytest.fixture()
def fake_hindsight():
    _FakeHindsightServer.captured_bodies = []
    server = HTTPServer(("127.0.0.1", 0), _FakeHindsightServer)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        thread.join(timeout=5)


def test_recall_context_returns_real_hindsight_results(monkeypatch, fake_daemon, fake_hindsight):
    _clear_memory_env(monkeypatch)
    monkeypatch.setenv("HINDSIGHT_BASE_URL", fake_hindsight)

    result = recall_context("greenhouse vent status", fake_daemon, "tok")

    assert "the greenhouse vent was opened last week" in result
    assert any(c["path"].endswith("/recall") for c in _FakeHindsightServer.captured_bodies)


def test_retain_outcome_sends_a_real_hindsight_retain_call(monkeypatch, fake_daemon, fake_hindsight):
    _clear_memory_env(monkeypatch)
    monkeypatch.setenv("HINDSIGHT_BASE_URL", fake_hindsight)

    retain_outcome("monitor the greenhouse", "vent opened at 60%", fake_daemon, "tok")

    retain_calls = [c for c in _FakeHindsightServer.captured_bodies if c["path"].endswith("/memories")]
    assert len(retain_calls) == 1
    content = retain_calls[0]["body"]["items"][0]["content"]
    assert "monitor the greenhouse" in content
    assert "vent opened at 60%" in content


def test_recall_context_propagates_a_real_graphiti_failure_not_configured_error(monkeypatch, fake_daemon):
    """Graphiti IS configured (GRAPHITI_KUZU_DB_PATH set) but the search
    call fails for a real, unrelated-to-configuration reason (the Kuzu
    FTS bug this test suite already documents). recall_context must let
    that propagate — silently treating a real outage as "not configured"
    would hide a genuine failure from the caller."""
    _clear_memory_env(monkeypatch)
    monkeypatch.setenv("GRAPHITI_KUZU_DB_PATH", ":memory:")

    with pytest.raises(RuntimeError, match="RelatesToNode_"):
        recall_context("greenhouse vent status", fake_daemon, "tok")


def test_retain_outcome_propagates_a_real_graphiti_failure(monkeypatch, fake_daemon):
    """retain_outcome partitions memory under a real group_id (_bank_id()),
    unlike test_memory_graph.py's add_episode test (which omits group_id
    entirely to sidestep this exact gap): graphiti-core's installed
    KuzuDriver never sets self._database, so add_episode's
    `group_id != self.driver._database` check AttributeErrors on any
    non-None group_id against Kuzu — a second, independent Kuzu-only gap
    alongside the missing FTS index (test_memory_graph.py). Neo4jDriver
    sets _database correctly. What's under test: this real failure
    propagates rather than being swallowed as "Graphiti just isn't
    configured"."""
    _clear_memory_env(monkeypatch)
    monkeypatch.setenv("GRAPHITI_KUZU_DB_PATH", ":memory:")

    with pytest.raises(AttributeError, match="_database"):
        retain_outcome("monitor the greenhouse", "vent opened", fake_daemon, "tok")
