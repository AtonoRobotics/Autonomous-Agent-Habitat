"""Tests for context/llm.py's ModelClient — a gRPC client to the daemon's
inference seam (daemon/grpcapi's InferenceService, Phase 4 of the gRPC
migration). These verify the real request/response handling against a
real in-process fake InferenceService (a real grpc.Server, not a mock —
not a live daemon, that's what test_control_plane_e2e.py-style fixtures
cover via the real Go binary); what's under test here is that this module
builds the right request, sends the right auth metadata, and never
substitutes a fake result on failure.
"""

from __future__ import annotations

from concurrent import futures

import grpc
import pytest

from context.inferencepb import inference_pb2, inference_pb2_grpc
from context.llm import ModelClient, ModelNotConfiguredError, from_env


def test_from_env_raises_without_adapter_model(monkeypatch):
    monkeypatch.delenv("ADAPTER_MODEL", raising=False)
    with pytest.raises(ModelNotConfiguredError):
        from_env("127.0.0.1:9", "agent-token")


def test_from_env_builds_client_from_model_and_provider_only(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "claude-sonnet-5")
    monkeypatch.setenv("ADAPTER_PROVIDER", "anthropic")
    client = from_env("127.0.0.1:9999", "agent-token-xyz")
    assert client.model == "claude-sonnet-5"
    assert client.provider == "anthropic"
    assert client.daemon_grpc_addr == "127.0.0.1:9999"
    assert client.agent_token == "agent-token-xyz"


def test_from_env_provider_defaults_to_empty_string(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "claude-sonnet-5")
    monkeypatch.delenv("ADAPTER_PROVIDER", raising=False)
    client = from_env("127.0.0.1:9999", "agent-token")
    assert client.provider == ""


def test_from_env_providers_defaults_to_none(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "claude-sonnet-5")
    monkeypatch.delenv("ADAPTER_PROVIDERS", raising=False)
    client = from_env("127.0.0.1:9999", "agent-token")
    assert client.providers is None


def test_from_env_parses_comma_separated_providers_failover_chain(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "claude-sonnet-5")
    monkeypatch.setenv("ADAPTER_PROVIDERS", "grok, anthropic ,glm")
    client = from_env("127.0.0.1:9999", "agent-token")
    assert client.providers == ["grok", "anthropic", "glm"]


class _FakeInferenceService(inference_pb2_grpc.InferenceServiceServicer):
    """Stands in for daemon/grpcapi's real InferenceService — a real
    grpc.Server-hosted servicer, not a mock. captured_* record the last
    real request this process received (and the raw incoming metadata,
    to assert on the auth header); response_error, when set, makes every
    RPC abort with that status instead of returning a canned response."""

    captured_request = None
    captured_metadata = None
    response_error: tuple | None = None  # (grpc.StatusCode, message)
    complete_response = None
    count_tokens_response = None
    embed_response = None

    def _record(self, request, context):
        cls = type(self)
        cls.captured_request = request
        cls.captured_metadata = dict(context.invocation_metadata())
        if cls.response_error is not None:
            context.abort(*cls.response_error)

    def Complete(self, request, context):
        self._record(request, context)
        return type(self).complete_response

    def CountTokens(self, request, context):
        self._record(request, context)
        return type(self).count_tokens_response

    def Embed(self, request, context):
        self._record(request, context)
        return type(self).embed_response


@pytest.fixture()
def fake_daemon():
    _FakeInferenceService.captured_request = None
    _FakeInferenceService.captured_metadata = None
    _FakeInferenceService.response_error = None
    _FakeInferenceService.complete_response = inference_pb2.CompleteResponse()
    _FakeInferenceService.count_tokens_response = inference_pb2.CountTokensResponse()
    _FakeInferenceService.embed_response = inference_pb2.EmbedResponse()

    server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
    inference_pb2_grpc.add_InferenceServiceServicer_to_server(_FakeInferenceService(), server)
    port = server.add_insecure_port("127.0.0.1:0")
    server.start()
    try:
        yield f"127.0.0.1:{port}"
    finally:
        server.stop(grace=1)


def test_complete_sends_real_request_and_parses_real_response(fake_daemon):
    _FakeInferenceService.complete_response = inference_pb2.CompleteResponse(text="the real answer")
    client = ModelClient(daemon_grpc_addr=fake_daemon, agent_token="my-agent-token", model="claude-sonnet-5", provider="anthropic")

    result = client.complete(system="be helpful", messages=[{"role": "user", "content": "hi"}])

    assert result == "the real answer"
    assert _FakeInferenceService.captured_metadata["authorization"] == "Bearer my-agent-token"
    req = _FakeInferenceService.captured_request
    assert req.provider == "anthropic"
    assert req.model == "claude-sonnet-5"
    assert req.system == "be helpful"
    assert [(m.role, m.content) for m in req.messages] == [("user", "hi")]


def test_complete_with_usage_returns_real_token_counts(fake_daemon):
    """§2.1/§14 cost accounting (AMH-LEDGER.md Tier 1): the daemon's
    Complete response now carries the provider's real input_tokens/
    output_tokens — complete_with_usage is the seam that surfaces them to
    a caller that wants to record real usage, without changing
    complete()'s existing plain-string return (many callers already
    depend on that shape)."""
    _FakeInferenceService.complete_response = inference_pb2.CompleteResponse(text="the real answer", input_tokens=123, output_tokens=45)
    client = ModelClient(daemon_grpc_addr=fake_daemon, agent_token="my-agent-token", model="claude-sonnet-5", provider="anthropic")

    result = client.complete_with_usage(system="be helpful", messages=[{"role": "user", "content": "hi"}])

    assert result.text == "the real answer"
    assert result.input_tokens == 123
    assert result.output_tokens == 45


def test_complete_with_usage_defaults_to_zero_when_daemon_omits_usage(fake_daemon):
    """A daemon build predating usage capture (or a provider response
    with no usage block) must not crash the caller — zero, not a
    fabricated guess."""
    _FakeInferenceService.complete_response = inference_pb2.CompleteResponse(text="ok")
    client = ModelClient(daemon_grpc_addr=fake_daemon, agent_token="tok", model="claude-sonnet-5")

    result = client.complete_with_usage(system="", messages=[{"role": "user", "content": "hi"}])

    assert result.input_tokens == 0
    assert result.output_tokens == 0


def test_complete_sends_providers_failover_chain_when_set(fake_daemon):
    _FakeInferenceService.complete_response = inference_pb2.CompleteResponse(text="the real answer")
    client = ModelClient(
        daemon_grpc_addr=fake_daemon, agent_token="tok", model="claude-sonnet-5",
        providers=["grok", "anthropic"],
    )

    client.complete(system="", messages=[{"role": "user", "content": "hi"}])

    assert list(_FakeInferenceService.captured_request.providers) == ["grok", "anthropic"]


def test_complete_sends_empty_providers_list_when_unset(fake_daemon):
    _FakeInferenceService.complete_response = inference_pb2.CompleteResponse(text="ok")
    client = ModelClient(daemon_grpc_addr=fake_daemon, agent_token="tok", model="claude-sonnet-5", provider="anthropic")

    client.complete(system="", messages=[{"role": "user", "content": "hi"}])

    assert list(_FakeInferenceService.captured_request.providers) == []


def test_count_tokens_sends_real_request_and_parses_real_response(fake_daemon):
    _FakeInferenceService.count_tokens_response = inference_pb2.CountTokensResponse(input_tokens=77)
    client = ModelClient(daemon_grpc_addr=fake_daemon, agent_token="tok", model="claude-sonnet-5")

    n = client.count_tokens(system="", messages=[{"role": "user", "content": "hi"}])

    assert n == 77


def test_complete_raises_on_daemon_error_response(fake_daemon):
    _FakeInferenceService.response_error = (grpc.StatusCode.NOT_FOUND, 'no active account for provider "anthropic"')
    client = ModelClient(daemon_grpc_addr=fake_daemon, agent_token="tok", model="claude-sonnet-5")

    with pytest.raises(ModelNotConfiguredError):
        client.complete(system="", messages=[{"role": "user", "content": "hi"}])


def test_complete_raises_when_daemon_unreachable():
    client = ModelClient(daemon_grpc_addr="127.0.0.1:1", agent_token="tok", model="claude-sonnet-5")
    with pytest.raises(ModelNotConfiguredError):
        client.complete(system="", messages=[{"role": "user", "content": "hi"}])


def test_from_env_embedding_fields_default_empty(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "claude-sonnet-5")
    monkeypatch.delenv("ADAPTER_EMBEDDING_MODEL", raising=False)
    monkeypatch.delenv("ADAPTER_EMBEDDING_PROVIDER", raising=False)
    monkeypatch.delenv("ADAPTER_EMBEDDING_PROVIDERS", raising=False)
    client = from_env("127.0.0.1:9999", "agent-token")
    assert client.embedding_model == ""
    assert client.embedding_provider == ""
    assert client.embedding_providers is None


def test_from_env_reads_embedding_fields(monkeypatch):
    monkeypatch.setenv("ADAPTER_MODEL", "claude-sonnet-5")
    monkeypatch.setenv("ADAPTER_EMBEDDING_MODEL", "text-embedding-3-small")
    monkeypatch.setenv("ADAPTER_EMBEDDING_PROVIDER", "openai")
    monkeypatch.setenv("ADAPTER_EMBEDDING_PROVIDERS", "openai, voyage")
    client = from_env("127.0.0.1:9999", "agent-token")
    assert client.embedding_model == "text-embedding-3-small"
    assert client.embedding_provider == "openai"
    assert client.embedding_providers == ["openai", "voyage"]


def test_embed_sends_real_request_and_parses_real_response(fake_daemon):
    _FakeInferenceService.embed_response = inference_pb2.EmbedResponse(
        embeddings=[
            inference_pb2.EmbedFloatVector(values=[0.1, 0.2]),
            inference_pb2.EmbedFloatVector(values=[0.3, 0.4]),
        ],
        dimension=2,
    )
    client = ModelClient(
        daemon_grpc_addr=fake_daemon, agent_token="my-agent-token", model="claude-sonnet-5",
        embedding_model="text-embedding-3-small", embedding_provider="openai",
    )

    result = client.embed(["first", "second"])

    assert len(result) == 2
    assert result[0] == pytest.approx([0.1, 0.2], abs=1e-6)
    assert result[1] == pytest.approx([0.3, 0.4], abs=1e-6)
    assert _FakeInferenceService.captured_metadata["authorization"] == "Bearer my-agent-token"
    req = _FakeInferenceService.captured_request
    assert req.provider == "openai"
    assert req.model == "text-embedding-3-small"
    assert list(req.input) == ["first", "second"]


def test_embed_raises_without_embedding_model_configured(fake_daemon):
    client = ModelClient(daemon_grpc_addr=fake_daemon, agent_token="tok", model="claude-sonnet-5")
    with pytest.raises(ModelNotConfiguredError):
        client.embed(["hi"])


def test_embed_raises_on_daemon_error_response(fake_daemon):
    _FakeInferenceService.response_error = (grpc.StatusCode.INVALID_ARGUMENT, "embeddings are only implemented for the openai_compatible provider kind")
    client = ModelClient(daemon_grpc_addr=fake_daemon, agent_token="tok", model="claude-sonnet-5", embedding_model="voyage-3")

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
