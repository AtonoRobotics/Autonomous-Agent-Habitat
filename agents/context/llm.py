"""Model client for AMH's cognition layer — an HTTP client to the daemon's
inference seam (daemon/inference, daemon/api's /v1/inference/* routes),
per docs/AMH-SPECIFICATION.md §2.1's "model-provider and tool-provider
seams" core responsibility.

This does NOT hold a model-provider credential. That is the point: an
agent computer (daemon/sandbox) is created and torn down constantly, and
authenticating each one individually against a real model provider isn't
viable — especially for a subscription OAuth session (Codex, Grok), which
is one refreshable login per account, not something to copy into every
ephemeral process. So the credential lives once, centrally, registered by
an operator as an account in daemon/credentials (exactly like a GitHub or
Gmail account — see the control-plane UI's Accounts tab) and this module
calls the daemon with only the same agent bearer token it already holds
for actuation, approval, and everything else — matching
agents/workflows/actuate.py's shape exactly.

complete() and count_tokens() make a real HTTP call and return the real
result. No provider registered on the daemon side -> the daemon returns
404 and this raises ModelNotConfiguredError — never a canned response.
"""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
from dataclasses import dataclass


class ModelNotConfiguredError(Exception):
    """No usable model provider is configured on the daemon, or the call
    itself failed. Callers must handle this — propagating a real failure,
    not substituting a fake result — is the entire point of this class
    existing separately from a generic exception."""


@dataclass
class ModelClient:
    """One agent's route to the daemon's inference seam. daemon_api_base_url
    and agent_token are the same values every other daemon-calling client
    in this codebase already threads through (see actuate.py, approval.py)
    — construct via from_env() for the model-name part only; the daemon
    connection details come from the same place they come from everywhere
    else in a workflow (the caller, ultimately the habitat that spawned
    this agent), never from this agent's own environment.
    """

    daemon_api_base_url: str
    agent_token: str
    model: str
    provider: str = ""

    def complete(self, system: str, messages: list[dict[str, str]], max_tokens: int = 4096) -> str:
        """Returns the model's real text response, via the daemon."""
        payload = {
            "provider": self.provider,
            "model": self.model,
            "system": system,
            "messages": messages,
            "max_tokens": max_tokens,
        }
        result = self._post("/v1/inference/complete", payload)
        return result["text"]

    def count_tokens(self, system: str, messages: list[dict[str, str]]) -> int:
        """Returns the provider's real input token count, via the daemon.
        Only implemented (daemon-side) for the anthropic provider."""
        payload = {"provider": self.provider, "model": self.model, "system": system, "messages": messages}
        result = self._post("/v1/inference/count-tokens", payload)
        return result["input_tokens"]

    def _post(self, path: str, payload: dict) -> dict:
        url = f"{self.daemon_api_base_url}{path}"
        request = urllib.request.Request(
            url,
            data=json.dumps(payload).encode("utf-8"),
            headers={"Content-Type": "application/json", "Authorization": f"Bearer {self.agent_token}"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(request, timeout=120) as response:
                return json.loads(response.read())
        except urllib.error.HTTPError as e:
            detail = e.read().decode("utf-8", errors="replace")
            raise ModelNotConfiguredError(f"inference call to {path} failed (HTTP {e.code}): {detail}") from e
        except urllib.error.URLError as e:
            raise ModelNotConfiguredError(f"could not reach the daemon at {url}: {e}") from e


def from_env(daemon_api_base_url: str, agent_token: str) -> ModelClient:
    """Builds a ModelClient for the model named by ADAPTER_MODEL (and
    optionally ADAPTER_PROVIDER — which registered daemon account to use;
    the daemon defaults to "anthropic" if omitted). Neither of these is a
    secret: choosing which model to ask for is a normal agent-run
    parameter, unlike the credential that authenticates the call, which
    this module never holds. Raises ModelNotConfiguredError if
    ADAPTER_MODEL is unset — every caller in this codebase must let that
    propagate, not catch it and substitute a fake result.
    """
    model = os.environ.get("ADAPTER_MODEL", "").strip()
    if not model:
        raise ModelNotConfiguredError("ADAPTER_MODEL is not set — no model is configured for this agent run")
    provider = os.environ.get("ADAPTER_PROVIDER", "").strip()
    return ModelClient(daemon_api_base_url=daemon_api_base_url, agent_token=agent_token, model=model, provider=provider)
