"""Model client for AMH's cognition layer — a gRPC client to the daemon's
inference seam (daemon/inference, daemon/grpcapi's InferenceService), per
docs/AMH-SPECIFICATION.md §2.1's "model-provider and tool-provider seams"
core responsibility.

Phase 4 (final phase) of the gRPC migration — see workflows/policy.py's
module docstring for Phase 1 and contracts/proto/inference.proto's header
comment for why this was migrated last: the hottest, highest-volume call
path in the daemon, with the largest blast radius, so every other
internal service moved first.

This does NOT hold a model-provider credential. That is the point: an
agent computer (daemon/sandbox) is created and torn down constantly, and
authenticating each one individually against a real model provider isn't
viable — especially for a subscription OAuth session (Codex, Grok), which
is one refreshable login per account, not something to copy into every
ephemeral process. So the credential lives once, centrally, registered by
an operator as an account in daemon/credentials (exactly like a GitHub or
Gmail account — see the control-plane UI's Accounts tab) and this module
calls the daemon with only the same agent bearer token it already holds
for policy/operations — matching agents/workflows/policy.py's shape
exactly.

complete(), count_tokens(), and embed() each make a real gRPC call and
return the real result. No provider registered on the daemon side -> the
daemon returns NOT_FOUND and this raises ModelNotConfiguredError — never
a canned response.
"""

from __future__ import annotations

import os
import threading
from dataclasses import dataclass

import grpc

from context.inferencepb import inference_pb2, inference_pb2_grpc


class ModelNotConfiguredError(Exception):
    """No usable model provider is configured on the daemon, or the call
    itself failed. Callers must handle this — propagating a real failure,
    not substituting a fake result — is the entire point of this class
    existing separately from a generic exception."""


@dataclass
class CompletionResult:
    """complete_with_usage()'s return: the model's real text plus the
    provider's own reported token counts. input_tokens/output_tokens
    default to 0, never fabricated, for a response that carried no
    usage block (an older daemon build, or a provider this codebase
    hasn't wired usage-parsing for yet)."""

    text: str
    input_tokens: int = 0
    output_tokens: int = 0


# See workflows/policy.py's identical _channels/_channel pattern: one
# grpc.Channel per distinct address, reused across calls/threads.
_channels: dict[str, grpc.Channel] = {}
_channels_lock = threading.Lock()


def _channel(daemon_grpc_addr: str) -> grpc.Channel:
    with _channels_lock:
        channel = _channels.get(daemon_grpc_addr)
        if channel is None:
            channel = grpc.insecure_channel(daemon_grpc_addr)
            _channels[daemon_grpc_addr] = channel
        return channel


@dataclass
class ModelClient:
    """One agent's route to the daemon's inference seam. daemon_grpc_addr
    and agent_token are the same values every other daemon-calling client
    in this codebase already threads through (see workflows/policy.py,
    workflows/operations.py) — construct via from_env() for the model-name
    part only; the daemon connection details come from the same place they
    come from everywhere else in a workflow (the caller, ultimately the
    habitat that spawned this agent), never from this agent's own
    environment.
    """

    daemon_grpc_addr: str
    agent_token: str
    model: str
    provider: str = ""
    providers: list[str] | None = None
    """Ordered failover chain of registered provider accounts (see
    daemon/inference's Request.Providers) — e.g. ["anthropic-prod",
    "anthropic-eval"] or ["grok", "anthropic"]. The daemon tries each in
    order and returns the first success. Takes precedence over `provider`
    when set; `provider` remains the single-provider shorthand."""

    embedding_model: str = ""
    embedding_provider: str = ""
    embedding_providers: list[str] | None = None
    """Separate from `model`/`provider`/`providers`: daemon/inference.Embed
    is only implemented for the openai_compatible provider kind (Anthropic
    has no first-party embeddings API), so a habitat whose completion
    provider is "anthropic" needs a distinct registered account — e.g.
    "voyage" or "openai" — for embed() to call."""

    def _stub(self) -> inference_pb2_grpc.InferenceServiceStub:
        return inference_pb2_grpc.InferenceServiceStub(_channel(self.daemon_grpc_addr))

    def _metadata(self) -> tuple:
        return (("authorization", f"Bearer {self.agent_token}"),)

    def complete(self, system: str, messages: list[dict[str, str]], max_tokens: int = 4096) -> str:
        """Returns the model's real text response, via the daemon."""
        return self._complete(system, messages, max_tokens).text

    def complete_with_usage(self, system: str, messages: list[dict[str, str]], max_tokens: int = 4096) -> CompletionResult:
        """Same real call as complete(), plus the provider's own reported
        token usage — see CompletionResult. A separate method rather than
        widening complete()'s return: complete() already has many callers
        that depend on its plain-string shape (decompose_goal,
        llm_summarize, the agentic loop's per-turn call), and none of them
        need usage — this is for the one real caller that does (agentic_loop.py,
        to accumulate real per-run token counts for §2.1/§14's cost
        accounting)."""
        return self._complete(system, messages, max_tokens)

    def _complete(self, system: str, messages: list[dict[str, str]], max_tokens: int) -> CompletionResult:
        req = inference_pb2.CompleteRequest(
            provider=self.provider, providers=self.providers or [], model=self.model,
            system=system, messages=[inference_pb2.Message(role=m["role"], content=m["content"]) for m in messages],
            max_tokens=max_tokens,
        )
        try:
            resp = self._stub().Complete(req, metadata=self._metadata(), timeout=120)
        except grpc.RpcError as e:
            raise ModelNotConfiguredError(f"inference Complete call failed: {e.details() or e.code().name}") from e
        return CompletionResult(text=resp.text, input_tokens=resp.input_tokens, output_tokens=resp.output_tokens)

    def count_tokens(self, system: str, messages: list[dict[str, str]]) -> int:
        """Returns the provider's real input token count, via the daemon.
        Only implemented (daemon-side) for the anthropic provider."""
        req = inference_pb2.CompleteRequest(
            provider=self.provider, providers=self.providers or [], model=self.model,
            system=system, messages=[inference_pb2.Message(role=m["role"], content=m["content"]) for m in messages],
        )
        try:
            resp = self._stub().CountTokens(req, metadata=self._metadata(), timeout=120)
        except grpc.RpcError as e:
            raise ModelNotConfiguredError(f"inference CountTokens call failed: {e.details() or e.code().name}") from e
        return resp.input_tokens

    def embed(self, texts: list[str]) -> list[list[float]]:
        """Returns one real embedding vector per entry in texts, in order,
        via the daemon. Raises ModelNotConfiguredError if embedding_model is
        unset or if the resolved provider account has no embeddings support
        (e.g. an "anthropic" kind credential — see embedding_model's doc
        comment above)."""
        if not self.embedding_model:
            raise ModelNotConfiguredError("embedding_model is not set — no embedding model is configured for this agent run")
        req = inference_pb2.EmbedRequest(
            provider=self.embedding_provider, providers=self.embedding_providers or [],
            model=self.embedding_model, input=texts,
        )
        try:
            resp = self._stub().Embed(req, metadata=self._metadata(), timeout=120)
        except grpc.RpcError as e:
            raise ModelNotConfiguredError(f"inference Embed call failed: {e.details() or e.code().name}") from e
        return [list(v.values) for v in resp.embeddings]


def from_env(daemon_grpc_addr: str, agent_token: str) -> ModelClient:
    """Builds a ModelClient for the model named by ADAPTER_MODEL (and
    optionally ADAPTER_PROVIDER — which registered daemon account to use;
    the daemon defaults to "anthropic" if omitted). ADAPTER_PROVIDERS, if
    set, is a comma-separated ordered failover chain (e.g.
    "anthropic-prod,anthropic-eval") and takes precedence over
    ADAPTER_PROVIDER. None of these is a secret: choosing which model/
    provider to ask for is a normal agent-run parameter, unlike the
    credential that authenticates the call, which this module never
    holds. Raises ModelNotConfiguredError if ADAPTER_MODEL is unset —
    every caller in this codebase must let that propagate, not catch it
    and substitute a fake result.
    """
    model = os.environ.get("ADAPTER_MODEL", "").strip()
    if not model:
        raise ModelNotConfiguredError("ADAPTER_MODEL is not set — no model is configured for this agent run")
    provider = os.environ.get("ADAPTER_PROVIDER", "").strip()
    providers_raw = os.environ.get("ADAPTER_PROVIDERS", "").strip()
    providers = [p.strip() for p in providers_raw.split(",") if p.strip()] or None

    embedding_model = os.environ.get("ADAPTER_EMBEDDING_MODEL", "").strip()
    embedding_provider = os.environ.get("ADAPTER_EMBEDDING_PROVIDER", "").strip()
    embedding_providers_raw = os.environ.get("ADAPTER_EMBEDDING_PROVIDERS", "").strip()
    embedding_providers = [p.strip() for p in embedding_providers_raw.split(",") if p.strip()] or None

    return ModelClient(
        daemon_grpc_addr=daemon_grpc_addr,
        agent_token=agent_token,
        model=model,
        provider=provider,
        providers=providers,
        embedding_model=embedding_model,
        embedding_provider=embedding_provider,
        embedding_providers=embedding_providers,
    )
