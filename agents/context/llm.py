"""Provider-neutral model client for AMH's cognition layer, per
docs/AMH-SPECIFICATION.md §1 decision 2 ("Python owns model-facing
cognition") and §2.1's "model-provider and tool-provider seams" core
responsibility.

This is the real thing, not a stand-in: complete() and count_tokens() make
real network calls to a real model provider and return the real result.
There is no local/offline mode that pretends to succeed — with no
provider configured, every function here raises ModelNotConfiguredError
rather than returning a canned response. Same fail-honest posture
daemon/credentials and daemon/authn already use for their own
configuration gates: a missing credential is a configuration error the
caller must handle, not something this module works around.

Anthropic is the default and only fully-implemented provider (ADAPTER_MODEL,
.env.example) — ANTHROPIC_API_KEY must be set. ADAPTER_BASE_URL selects an
OpenAI-compatible chat-completions endpoint instead (e.g. DeepSeek, per
.env.example's existing comment) via a plain HTTP call, matching this
codebase's established urllib.request convention (agents/workflows/
approval.py, safetycase.py, actuate.py) rather than adding a second SDK
dependency for a secondary path.
"""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
from dataclasses import dataclass


class ModelNotConfiguredError(Exception):
    """No usable model provider is configured, or the provider call
    itself failed. Callers must handle this — propagating a real failure,
    not substituting a fake result — is the entire point of this class
    existing separately from a generic exception."""


@dataclass
class ModelClient:
    """One configured model endpoint. Construct via from_env(), not
    directly, so ADAPTER_MODEL/ADAPTER_BASE_URL/ANTHROPIC_API_KEY stay the
    single source of truth for what "configured" means."""

    provider: str  # "anthropic" | "openai_compatible"
    model: str
    api_key: str
    base_url: str | None = None

    def complete(self, system: str, messages: list[dict[str, str]], max_tokens: int = 4096) -> str:
        """Returns the model's real text response. Raises
        ModelNotConfiguredError on any provider-side failure (auth,
        network, malformed response) — never returns a placeholder."""
        if self.provider == "anthropic":
            return self._complete_anthropic(system, messages, max_tokens)
        return self._complete_openai_compatible(system, messages, max_tokens)

    def count_tokens(self, system: str, messages: list[dict[str, str]]) -> int:
        """Returns the provider's real input token count for this
        system+messages — the actual number the model will see, not an
        estimate. Only implemented for the anthropic provider: an
        OpenAI-compatible chat-completions endpoint has no standardized
        token-count API to call instead."""
        if self.provider != "anthropic":
            raise ModelNotConfiguredError(
                "count_tokens is only implemented for the anthropic provider; "
                "the configured ADAPTER_BASE_URL endpoint has no standard token-count API"
            )
        import anthropic

        client = anthropic.Anthropic(api_key=self.api_key, base_url=self.base_url)
        try:
            result = client.messages.count_tokens(
                model=self.model,
                system=system or anthropic.NOT_GIVEN,
                messages=messages,
            )
        except anthropic.APIError as e:
            raise ModelNotConfiguredError(f"count_tokens call failed: {e}") from e
        return result.input_tokens

    def _complete_anthropic(self, system: str, messages: list[dict[str, str]], max_tokens: int) -> str:
        import anthropic

        client = anthropic.Anthropic(api_key=self.api_key, base_url=self.base_url)
        try:
            response = client.messages.create(
                model=self.model,
                max_tokens=max_tokens,
                system=system or anthropic.NOT_GIVEN,
                messages=messages,
            )
        except anthropic.APIError as e:
            raise ModelNotConfiguredError(f"model call failed: {e}") from e
        return "".join(block.text for block in response.content if block.type == "text")

    def _complete_openai_compatible(self, system: str, messages: list[dict[str, str]], max_tokens: int) -> str:
        full_messages = ([{"role": "system", "content": system}] if system else []) + list(messages)
        body = json.dumps({"model": self.model, "messages": full_messages, "max_tokens": max_tokens}).encode("utf-8")
        request = urllib.request.Request(
            self.base_url.rstrip("/") + "/chat/completions",
            data=body,
            headers={"Content-Type": "application/json", "Authorization": f"Bearer {self.api_key}"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(request, timeout=120) as response:
                payload = json.loads(response.read())
        except urllib.error.HTTPError as e:
            detail = e.read().decode("utf-8", errors="replace")
            raise ModelNotConfiguredError(f"model provider returned HTTP {e.code}: {detail}") from e
        except urllib.error.URLError as e:
            raise ModelNotConfiguredError(f"could not reach model provider at {self.base_url}: {e}") from e
        try:
            return payload["choices"][0]["message"]["content"]
        except (KeyError, IndexError) as e:
            raise ModelNotConfiguredError(f"unexpected response shape from model provider: {payload}") from e


def from_env() -> ModelClient:
    """Builds a ModelClient from ADAPTER_MODEL / ADAPTER_BASE_URL, and
    whichever credential matches the selected provider (.env.example).
    Raises ModelNotConfiguredError if nothing usable is configured — every
    caller in this codebase must let that propagate, not catch it and
    substitute a fake result.

    ADAPTER_BASE_URL set -> ADAPTER_API_KEY (a generic bearer credential:
    the endpoint is provider-neutral, e.g. DeepSeek, so naming it
    ANTHROPIC_API_KEY would be actively wrong). No ADAPTER_BASE_URL ->
    ANTHROPIC_API_KEY, matching the Anthropic SDK's own convention.
    """
    model = os.environ.get("ADAPTER_MODEL", "").strip()
    base_url = os.environ.get("ADAPTER_BASE_URL", "").strip() or None

    if not model:
        raise ModelNotConfiguredError("ADAPTER_MODEL is not set — no model is configured for this agent run")

    if base_url:
        api_key = os.environ.get("ADAPTER_API_KEY", "").strip()
        if not api_key:
            raise ModelNotConfiguredError(
                "ADAPTER_BASE_URL is set but ADAPTER_API_KEY is empty — an "
                "OpenAI-compatible endpoint still needs a bearer credential"
            )
        return ModelClient(provider="openai_compatible", model=model, api_key=api_key, base_url=base_url)

    api_key = os.environ.get("ANTHROPIC_API_KEY", "").strip()
    if not api_key:
        raise ModelNotConfiguredError("ANTHROPIC_API_KEY is not set — cannot call the Anthropic API")
    return ModelClient(provider="anthropic", model=model, api_key=api_key)
