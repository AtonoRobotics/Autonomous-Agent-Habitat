"""Generic external-effect-lifecycle HTTP client over daemon/api's
/v1/operations/* routes (daemon/operations — docs/AMH-SPECIFICATION.md §4).

Deliberately plain functions, not @DBOS.step()-decorated like
workflows/policy.py's decide()/consume(). policy.py's step decoration was
speculative — written before it had a real call site. This module's real
call site (harness/agentic_loop.py's MCP call site, called from
workflows/goal.py's do_subagent_work) already runs entirely inside one
@DBOS.step(): do_subagent_work calls run_agentic_loop synchronously, which
runs the whole loop body to completion before returning. Nesting a
DBOS.step() call inside an already-executing step isn't a meaningful unit
of durability there — it would just be a plain HTTP call with extra
bookkeeping DBOS doesn't apply mid-step. That matches how
context/llm.py's ModelClient already calls the daemon's inference routes
from the very same call graph: plain HTTP, no step decoration. A future
call site invoking this module directly from workflow-level code (outside
any step) is free to wrap these calls in DBOS.step() at that call site.

Track-only, not enforcing: the built-in policy (daemon/policy's
DefaultPolicyID) admits only Reversibility "verified" — an attested
inverse. A generic third-party MCP tool call has no such attestation (the
harness has no way to verify an arbitrary tool has a reverse action), so
propose() here always passes reversibility "none", which legitimately
resolves to needs_approval and stays there. Callers do not gate tool
execution on the resulting decision — the effect record exists so an
interrupted or failed call is visible and reconcilable (§4, invariant #2),
not to block calls today. Enforcing admission before executing an MCP
call is a real, separate step this module deliberately does not take yet;
see harness/agentic_loop.py's doc comment.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request

from context.observability import tool_call_span


class OperationsError(Exception):
    pass


def _error_message(e: urllib.error.HTTPError) -> str:
    """See workflows/policy.py's _error_message: daemon/authn's
    RequireRole middleware can reject a request before any /v1/operations
    handler runs, via plain http.Error (plain text, not JSON)."""
    body = e.read()
    try:
        return json.loads(body).get("error", f"HTTP {e.code}")
    except json.JSONDecodeError:
        return body.decode("utf-8", errors="replace").strip() or f"HTTP {e.code}"


def _post(url: str, token: str, body: dict) -> dict:
    request = urllib.request.Request(
        url,
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as e:
        raise OperationsError(_error_message(e)) from e


def _get(url: str, token: str) -> dict:
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}"})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as e:
        raise OperationsError(_error_message(e)) from e


def propose(
    daemon_api_base_url: str,
    agent_token: str,
    operation_id: str,
    owner_extension_id: str,
    effect_type: str,
    payload: dict,
    reversibility: str = "none",
) -> dict:
    """Proposes a new effect record for operation_id. Returns the Effect
    (see contracts/effect-record.schema.json): state "admitted" or
    "needs_approval" per daemon/policy's built-in decision, never "denied"
    for this generic policy (see daemon/policy.Decide's doc comment)."""
    url = f"{daemon_api_base_url}/v1/operations"
    body = {
        "operation_id": operation_id,
        "owner_extension_id": owner_extension_id,
        "effect_type": effect_type,
        "payload": payload,
        "reversibility": reversibility,
    }
    with tool_call_span("operations:propose", **{"amh.api.url": url}):
        return _post(url, agent_token, body)


def mark_dispatch_pending(daemon_api_base_url: str, agent_token: str, effect_id: str) -> dict:
    url = f"{daemon_api_base_url}/v1/operations/{effect_id}/dispatch-pending"
    with tool_call_span("operations:dispatch_pending", **{"amh.api.url": url}):
        return _post(url, agent_token, {})


def mark_dispatched(daemon_api_base_url: str, agent_token: str, effect_id: str, external_command_id: str = "") -> dict:
    url = f"{daemon_api_base_url}/v1/operations/{effect_id}/dispatched"
    body = {"external_command_id": external_command_id} if external_command_id else {}
    with tool_call_span("operations:dispatched", **{"amh.api.url": url}):
        return _post(url, agent_token, body)


def mark_observed(daemon_api_base_url: str, agent_token: str, effect_id: str, observation_ref: str = "") -> dict:
    url = f"{daemon_api_base_url}/v1/operations/{effect_id}/observed"
    body = {"observation_ref": observation_ref} if observation_ref else {}
    with tool_call_span("operations:observed", **{"amh.api.url": url}):
        return _post(url, agent_token, body)


def resolve(
    daemon_api_base_url: str,
    agent_token: str,
    effect_id: str,
    terminal: str,
    error_code: str = "",
    error_retryable: bool = False,
    error_message: str = "",
) -> dict:
    """terminal is one of "confirmed"/"reconciled"/"compensated"/"failed"
    — the caller's own verdict, trusted as-is by daemon/operations.Resolve
    for external-effect outcomes specifically (docs/AMH-SPECIFICATION.md
    §4: the core "SHALL NOT infer that [an external] effect failed"), the
    deliberate exception to this codebase's usual never-trust-the-caller
    pattern (contrast daemon/selfimprove's server-computed eval verdict)."""
    url = f"{daemon_api_base_url}/v1/operations/{effect_id}/resolve"
    body: dict = {"terminal": terminal}
    if error_code or error_message:
        body["error_code"] = error_code
        body["retryable"] = error_retryable
        body["message"] = error_message
    with tool_call_span("operations:resolve", **{"amh.api.url": url}):
        return _post(url, agent_token, body)


def get_effect(daemon_api_base_url: str, agent_token: str, effect_id: str) -> dict:
    """A cheap, idempotent read."""
    return _get(f"{daemon_api_base_url}/v1/operations/{effect_id}", agent_token)


def list_effects_by_operation(daemon_api_base_url: str, agent_token: str, operation_id: str) -> list[dict]:
    """A cheap, idempotent read of every effect proposed under operation_id."""
    return _get(f"{daemon_api_base_url}/v1/operations?operation_id={operation_id}", agent_token)
