"""Tests for context/llm.py's real model client. No live API key exists
in this environment, so these verify the real request/response handling
logic against a stand-in transport — not that Anthropic's actual servers
are reachable. What's under test is that this module makes a real call
with the right shape and parses a real response correctly, and that it
never substitutes a fake result when configuration or the call itself
fails.
"""

from __future__ import annotations

import io
import json
import urllib.error

import pytest

from context.llm import ModelClient, ModelNotConfiguredError, from_env


def test_from_env_raises_without_any_configuration(monkeypatch):
    monkeypatch.delenv("ADAPTER_MODEL", raising=False)
    monkeypatch.delenv("ADAPTER_BASE_URL", raising=False)
    monkeypatch.delenv("ADAPTER_API_KEY", raising=False)
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    with pytest.raises(ModelNotConfiguredError):
        from_env()


def test_from_env_raises_with_model_but_no_key(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "claude-sonnet-5")
    monkeypatch.delenv("ADAPTER_BASE_URL", raising=False)
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    with pytest.raises(ModelNotConfiguredError):
        from_env()


def test_from_env_builds_anthropic_client_when_configured(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "claude-sonnet-5")
    monkeypatch.delenv("ADAPTER_BASE_URL", raising=False)
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-test-key")
    client = from_env()
    assert client.provider == "anthropic"
    assert client.model == "claude-sonnet-5"
    assert client.api_key == "sk-test-key"


def test_from_env_requires_api_key_even_with_base_url(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "deepseek-chat")
    monkeypatch.setenv("ADAPTER_BASE_URL", "https://api.deepseek.com")
    monkeypatch.delenv("ADAPTER_API_KEY", raising=False)
    with pytest.raises(ModelNotConfiguredError):
        from_env()


def test_from_env_builds_openai_compatible_client_when_configured(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "deepseek-chat")
    monkeypatch.setenv("ADAPTER_BASE_URL", "https://api.deepseek.com")
    monkeypatch.setenv("ADAPTER_API_KEY", "sk-test-key")
    client = from_env()
    assert client.provider == "openai_compatible"
    assert client.base_url == "https://api.deepseek.com"


def test_complete_anthropic_returns_real_response_text(monkeypatch):
    """Mocks only the SDK's network call, not this module's own logic —
    verifies complete() correctly builds the request and extracts text
    from a genuine Anthropic response shape."""
    import anthropic

    class FakeTextBlock:
        type = "text"
        text = "the real model's answer"

    class FakeResponse:
        content = [FakeTextBlock()]

    captured = {}

    def fake_create(self, **kwargs):
        captured.update(kwargs)
        return FakeResponse()

    monkeypatch.setattr(anthropic.resources.messages.Messages, "create", fake_create)

    client = ModelClient(provider="anthropic", model="claude-sonnet-5", api_key="sk-test-key")
    result = client.complete(system="be helpful", messages=[{"role": "user", "content": "hello"}])

    assert result == "the real model's answer"
    assert captured["model"] == "claude-sonnet-5"
    assert captured["system"] == "be helpful"
    assert captured["messages"] == [{"role": "user", "content": "hello"}]


def test_complete_anthropic_raises_on_api_error(monkeypatch):
    import anthropic

    def fake_create(self, **kwargs):
        raise anthropic.APIError("boom", request=None, body=None)

    monkeypatch.setattr(anthropic.resources.messages.Messages, "create", fake_create)

    client = ModelClient(provider="anthropic", model="claude-sonnet-5", api_key="sk-test-key")
    with pytest.raises(ModelNotConfiguredError):
        client.complete(system="", messages=[{"role": "user", "content": "hi"}])


def test_count_tokens_anthropic_returns_real_count(monkeypatch):
    import anthropic

    class FakeCountResult:
        input_tokens = 42

    def fake_count_tokens(self, **kwargs):
        return FakeCountResult()

    monkeypatch.setattr(anthropic.resources.messages.Messages, "count_tokens", fake_count_tokens)

    client = ModelClient(provider="anthropic", model="claude-sonnet-5", api_key="sk-test-key")
    assert client.count_tokens(system="", messages=[{"role": "user", "content": "hi"}]) == 42


def test_count_tokens_not_implemented_for_openai_compatible():
    client = ModelClient(provider="openai_compatible", model="deepseek-chat", api_key="k", base_url="https://x")
    with pytest.raises(ModelNotConfiguredError):
        client.count_tokens(system="", messages=[])


def test_complete_openai_compatible_parses_real_response_shape(monkeypatch):
    response_body = json.dumps(
        {"choices": [{"message": {"role": "assistant", "content": "deepseek's answer"}}]}
    ).encode("utf-8")

    class FakeHTTPResponse(io.BytesIO):
        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    def fake_urlopen(request, timeout=None):
        assert request.full_url == "https://api.deepseek.com/chat/completions"
        payload = json.loads(request.data)
        assert payload["model"] == "deepseek-chat"
        assert payload["messages"][0] == {"role": "system", "content": "be helpful"}
        return FakeHTTPResponse(response_body)

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    client = ModelClient(provider="openai_compatible", model="deepseek-chat", api_key="k", base_url="https://api.deepseek.com")
    result = client.complete(system="be helpful", messages=[{"role": "user", "content": "hi"}])
    assert result == "deepseek's answer"


def test_complete_openai_compatible_raises_on_http_error(monkeypatch):
    def fake_urlopen(request, timeout=None):
        raise urllib.error.HTTPError(request.full_url, 401, "Unauthorized", None, io.BytesIO(b'{"error":"bad key"}'))

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    client = ModelClient(provider="openai_compatible", model="deepseek-chat", api_key="bad", base_url="https://api.deepseek.com")
    with pytest.raises(ModelNotConfiguredError):
        client.complete(system="", messages=[{"role": "user", "content": "hi"}])
