"""Tests for context/llm.py's ModelClient — an HTTP client to the daemon's
inference seam. These verify the real request/response handling against a
stand-in HTTP server playing the daemon's part (not a live daemon — that's
what test_control_plane_e2e.py-style fixtures cover via the real Go
binary); what's under test here is that this module builds the right
request, sends the right auth header, and never substitutes a fake result
on failure.
"""

from __future__ import annotations

import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from threading import Thread

import pytest

from context.llm import ModelClient, ModelNotConfiguredError, from_env


def test_from_env_raises_without_adapter_model(monkeypatch):
    monkeypatch.delenv("ADAPTER_MODEL", raising=False)
    with pytest.raises(ModelNotConfiguredError):
        from_env("http://127.0.0.1:9", "agent-token")


def test_from_env_builds_client_from_model_and_provider_only(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "claude-sonnet-5")
    monkeypatch.setenv("ADAPTER_PROVIDER", "anthropic")
    client = from_env("http://127.0.0.1:9999", "agent-token-xyz")
    assert client.model == "claude-sonnet-5"
    assert client.provider == "anthropic"
    assert client.daemon_api_base_url == "http://127.0.0.1:9999"
    assert client.agent_token == "agent-token-xyz"


def test_from_env_provider_defaults_to_empty_string(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "claude-sonnet-5")
    monkeypatch.delenv("ADAPTER_PROVIDER", raising=False)
    client = from_env("http://127.0.0.1:9999", "agent-token")
    assert client.provider == ""


def test_from_env_providers_defaults_to_none(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "claude-sonnet-5")
    monkeypatch.delenv("ADAPTER_PROVIDERS", raising=False)
    client = from_env("http://127.0.0.1:9999", "agent-token")
    assert client.providers is None


def test_from_env_parses_comma_separated_providers_failover_chain(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "claude-sonnet-5")
    monkeypatch.setenv("ADAPTER_PROVIDERS", "grok, anthropic ,glm")
    client = from_env("http://127.0.0.1:9999", "agent-token")
    assert client.providers == ["grok", "anthropic", "glm"]


class _FakeDaemon(BaseHTTPRequestHandler):
    captured_path = None
    captured_auth = None
    captured_body = None
    response_status = 200
    response_body = b"{}"

    def log_message(self, format, *args):
        pass

    def do_POST(self):
        type(self).captured_path = self.path
        type(self).captured_auth = self.headers.get("Authorization")
        length = int(self.headers.get("Content-Length", 0))
        type(self).captured_body = json.loads(self.rfile.read(length))
        self.send_response(type(self).response_status)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(type(self).response_body)


@pytest.fixture()
def fake_daemon():
    server = HTTPServer(("127.0.0.1", 0), _FakeDaemon)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    _FakeDaemon.response_status = 200
    _FakeDaemon.response_body = b"{}"
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        thread.join(timeout=5)


def test_complete_sends_real_request_and_parses_real_response(fake_daemon):
    _FakeDaemon.response_body = json.dumps({"text": "the real answer"}).encode()
    client = ModelClient(daemon_api_base_url=fake_daemon, agent_token="my-agent-token", model="claude-sonnet-5", provider="anthropic")

    result = client.complete(system="be helpful", messages=[{"role": "user", "content": "hi"}])

    assert result == "the real answer"
    assert _FakeDaemon.captured_path == "/v1/inference/complete"
    assert _FakeDaemon.captured_auth == "Bearer my-agent-token"
    assert _FakeDaemon.captured_body["provider"] == "anthropic"
    assert _FakeDaemon.captured_body["model"] == "claude-sonnet-5"
    assert _FakeDaemon.captured_body["system"] == "be helpful"
    assert _FakeDaemon.captured_body["messages"] == [{"role": "user", "content": "hi"}]


def test_complete_sends_providers_failover_chain_when_set(fake_daemon):
    _FakeDaemon.response_body = json.dumps({"text": "the real answer"}).encode()
    client = ModelClient(
        daemon_api_base_url=fake_daemon, agent_token="tok", model="claude-sonnet-5",
        providers=["grok", "anthropic"],
    )

    client.complete(system="", messages=[{"role": "user", "content": "hi"}])

    assert _FakeDaemon.captured_body["providers"] == ["grok", "anthropic"]


def test_complete_sends_empty_providers_list_when_unset(fake_daemon):
    _FakeDaemon.response_body = json.dumps({"text": "ok"}).encode()
    client = ModelClient(daemon_api_base_url=fake_daemon, agent_token="tok", model="claude-sonnet-5", provider="anthropic")

    client.complete(system="", messages=[{"role": "user", "content": "hi"}])

    assert _FakeDaemon.captured_body["providers"] == []


def test_count_tokens_sends_real_request_and_parses_real_response(fake_daemon):
    _FakeDaemon.response_body = json.dumps({"input_tokens": 77}).encode()
    client = ModelClient(daemon_api_base_url=fake_daemon, agent_token="tok", model="claude-sonnet-5")

    n = client.count_tokens(system="", messages=[{"role": "user", "content": "hi"}])

    assert n == 77
    assert _FakeDaemon.captured_path == "/v1/inference/count-tokens"


def test_complete_raises_on_daemon_error_response(fake_daemon):
    _FakeDaemon.response_status = 404
    _FakeDaemon.response_body = json.dumps({"error": "no active account for provider \"anthropic\""}).encode()
    client = ModelClient(daemon_api_base_url=fake_daemon, agent_token="tok", model="claude-sonnet-5")

    with pytest.raises(ModelNotConfiguredError):
        client.complete(system="", messages=[{"role": "user", "content": "hi"}])


def test_complete_raises_when_daemon_unreachable():
    client = ModelClient(daemon_api_base_url="http://127.0.0.1:1", agent_token="tok", model="claude-sonnet-5")
    with pytest.raises(ModelNotConfiguredError):
        client.complete(system="", messages=[{"role": "user", "content": "hi"}])


def test_from_env_embedding_fields_default_empty(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "claude-sonnet-5")
    monkeypatch.delenv("ADAPTER_EMBEDDING_MODEL", raising=False)
    monkeypatch.delenv("ADAPTER_EMBEDDING_PROVIDER", raising=False)
    monkeypatch.delenv("ADAPTER_EMBEDDING_PROVIDERS", raising=False)
    client = from_env("http://127.0.0.1:9999", "agent-token")
    assert client.embedding_model == ""
    assert client.embedding_provider == ""
    assert client.embedding_providers is None


def test_from_env_reads_embedding_fields(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "claude-sonnet-5")
    monkeypatch.setenv("ADAPTER_EMBEDDING_MODEL", "text-embedding-3-small")
    monkeypatch.setenv("ADAPTER_EMBEDDING_PROVIDER", "openai")
    monkeypatch.setenv("ADAPTER_EMBEDDING_PROVIDERS", "openai, voyage")
    client = from_env("http://127.0.0.1:9999", "agent-token")
    assert client.embedding_model == "text-embedding-3-small"
    assert client.embedding_provider == "openai"
    assert client.embedding_providers == ["openai", "voyage"]


def test_embed_sends_real_request_and_parses_real_response(fake_daemon):
    _FakeDaemon.response_body = json.dumps({"embeddings": [[0.1, 0.2], [0.3, 0.4]], "dimension": 2}).encode()
    client = ModelClient(
        daemon_api_base_url=fake_daemon, agent_token="my-agent-token", model="claude-sonnet-5",
        embedding_model="text-embedding-3-small", embedding_provider="openai",
    )

    result = client.embed(["first", "second"])

    assert result == [[0.1, 0.2], [0.3, 0.4]]
    assert _FakeDaemon.captured_path == "/v1/inference/embed"
    assert _FakeDaemon.captured_auth == "Bearer my-agent-token"
    assert _FakeDaemon.captured_body["provider"] == "openai"
    assert _FakeDaemon.captured_body["model"] == "text-embedding-3-small"
    assert _FakeDaemon.captured_body["input"] == ["first", "second"]


def test_embed_raises_without_embedding_model_configured(fake_daemon):
    client = ModelClient(daemon_api_base_url=fake_daemon, agent_token="tok", model="claude-sonnet-5")
    with pytest.raises(ModelNotConfiguredError):
        client.embed(["hi"])


def test_embed_raises_on_daemon_error_response(fake_daemon):
    _FakeDaemon.response_status = 400
    _FakeDaemon.response_body = json.dumps({"error": "embeddings are only implemented for the openai_compatible provider kind"}).encode()
    client = ModelClient(daemon_api_base_url=fake_daemon, agent_token="tok", model="claude-sonnet-5", embedding_model="voyage-3")

    with pytest.raises(ModelNotConfiguredError):
        client.embed(["hi"])


def test_never_sends_a_model_provider_credential():
    """Structural guardrail: this module has no parameter or attribute
    shaped like a model-provider secret — only the daemon holds one."""
    import inspect

    import context.llm as llm_module

    for name in ("ModelClient", "from_env"):
        obj = getattr(llm_module, name)
        params = set(inspect.signature(obj).parameters) if inspect.isfunction(obj) else set(inspect.signature(obj.__init__).parameters)
        for suspicious in ("api_key", "apikey", "secret", "credential"):
            assert not any(suspicious in p.lower() for p in params), f"{name} accepts a parameter shaped like a model-provider credential: {params}"
