"""Tests for memory/episodic.py's hindsight_from_env — construction only
(matching test_llm.py's scope for ModelClient). Hindsight itself is a
separate service process (hindsight-api-slim), not a dependency of
agents/ — see memory/episodic.py's module docstring for how this was
verified against a real, live instance outside this test suite.
"""

from __future__ import annotations

import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from threading import Thread

import pytest

from memory.episodic import HindsightNotConfiguredError, hindsight_from_env


def test_hindsight_from_env_raises_without_base_url(monkeypatch):
    monkeypatch.delenv("HINDSIGHT_BASE_URL", raising=False)
    with pytest.raises(HindsightNotConfiguredError):
        hindsight_from_env()


def test_hindsight_from_env_builds_client_from_base_url(monkeypatch):
    monkeypatch.setenv("HINDSIGHT_BASE_URL", "http://127.0.0.1:9999")
    monkeypatch.delenv("HINDSIGHT_API_KEY", raising=False)
    client = hindsight_from_env()
    assert client._base_url == "http://127.0.0.1:9999"
    assert client._api_key is None


def test_hindsight_from_env_reads_api_key(monkeypatch):
    monkeypatch.setenv("HINDSIGHT_BASE_URL", "http://127.0.0.1:9999")
    monkeypatch.setenv("HINDSIGHT_API_KEY", "secret-key")
    client = hindsight_from_env()
    assert client._api_key == "secret-key"


class _FakeHindsightServer(BaseHTTPRequestHandler):
    captured_path = None
    captured_auth = None

    def log_message(self, format, *args):
        pass

    def do_POST(self):
        type(self).captured_path = self.path
        type(self).captured_auth = self.headers.get("Authorization")
        length = int(self.headers.get("Content-Length", 0))
        self.rfile.read(length)
        body = json.dumps({"success": True, "bank_id": "b", "items_count": 1, "async": False}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body)


@pytest.fixture()
def fake_hindsight_server():
    server = HTTPServer(("127.0.0.1", 0), _FakeHindsightServer)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        thread.join(timeout=5)


def test_client_sends_real_request_with_configured_api_key(monkeypatch, fake_hindsight_server):
    monkeypatch.setenv("HINDSIGHT_BASE_URL", fake_hindsight_server)
    monkeypatch.setenv("HINDSIGHT_API_KEY", "my-hindsight-key")
    client = hindsight_from_env()

    import datetime

    result = client.retain(bank_id="test-bank", content="a fact", timestamp=datetime.datetime.now(datetime.timezone.utc))

    assert result.success is True
    assert _FakeHindsightServer.captured_auth == "Bearer my-hindsight-key"
