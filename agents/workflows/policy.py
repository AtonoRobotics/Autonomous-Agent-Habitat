"""Generic policy/approval HTTP client over daemon/api's /v1/policy/*
routes (daemon/policy — docs/AMH-SPECIFICATION.md §6).

decide()/consume() are agent-token: proposing an action and, once
admitted, dispatching it are exactly the "agents propose" half of
decision 9 that daemon/api's RBAC table allows an agent token to do.
approve()/deny() take an operator_token instead — the daemon mechanically
refuses an agent token on those routes (403), the same anti-self-approval
property every other operator-only route enforces. A workflow that
reaches needs_approval cannot resolve its own request; it has to wait for
an operator to call approve()/deny() out-of-band (e.g. from the
control-plane UI) and can poll get_decision()/get_approval_request() to
learn the outcome.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request

from dbos import DBOS

from context.observability import tool_call_span


class PolicyError(Exception):
    pass


def _error_message(e: urllib.error.HTTPError) -> str:
    """Extracts the error message from an HTTPError body. Most /v1/policy/*
    handlers reply with a JSON {"error": "..."} body, but daemon/authn's
    RequireRole middleware rejects an unauthorized/forbidden request
    before any handler runs, via plain http.Error — plain text, not JSON.
    Falling back to that raw text (rather than letting json.loads raise
    past this function) is what makes the 403 an agent token gets for
    calling approve()/deny() surface as a PolicyError like everything
    else, not an unrelated JSONDecodeError."""
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
        raise PolicyError(_error_message(e)) from e


def _get(url: str, token: str) -> dict:
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}"})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as e:
        raise PolicyError(_error_message(e)) from e


@DBOS.step()
def decide(daemon_api_base_url: str, agent_token: str, operation_id: str, payload: dict, reversibility: str) -> dict:
    """Proposes operation_id/payload to the policy hook. reversibility is
    "verified", "claimed", or "none" (contracts/action-envelope.schema.json's
    properties.reversibility.status) — the one generic property
    daemon/policy's built-in policy evaluates. Returns the PolicyDecision:
    result "admit"/"admit_with_constraints" means consume() may proceed;
    "needs_approval" means wait on approval_request_id via
    get_approval_request() or get_decision(); "deny"/"defer" mean stop. A
    @DBOS.step() — the decision itself is a durably recorded fact, not a
    cheap read."""
    url = f"{daemon_api_base_url}/v1/policy/decide"
    body = {"operation_id": operation_id, "payload": payload, "reversibility": reversibility}
    with tool_call_span("policy:decide", **{"amh.api.url": url}):
        return _post(url, agent_token, body)


@DBOS.step()
def consume(daemon_api_base_url: str, agent_token: str, decision_id: str, payload: dict) -> None:
    """Atomically single-uses decision_id right before dispatching payload
    — payload MUST be byte-for-byte the same value passed to decide(); the
    daemon recomputes its digest and refuses to consume a decision bound
    to a different payload (policy.ErrDigestMismatch), not just a
    different decision_id. Raises PolicyError on any non-2xx: already
    consumed, expired, not admitted, or digest mismatch — every one of
    those means "do not dispatch," which is why this has no return value
    for callers to ignore."""
    url = f"{daemon_api_base_url}/v1/policy/decisions/{decision_id}/consume"
    with tool_call_span("policy:consume", **{"amh.api.url": url}):
        _post(url, agent_token, {"payload": payload})


def get_decision(daemon_api_base_url: str, agent_token: str, decision_id: str) -> dict:
    """Not a @DBOS.step(): a cheap, idempotent read — for a workflow
    polling a needs_approval decision's fate, or checking whether a
    decision it already holds has since been consumed."""
    return _get(f"{daemon_api_base_url}/v1/policy/decisions/{decision_id}", agent_token)


def list_pending_approvals(daemon_api_base_url: str, agent_token: str) -> list[dict]:
    """Not a @DBOS.step(): a cheap, idempotent read of every ApprovalRequest
    still awaiting operator resolution — the queue an operator surface
    polls or lists."""
    return _get(f"{daemon_api_base_url}/v1/policy/approvals", agent_token)


def get_approval_request(daemon_api_base_url: str, agent_token: str, approval_request_id: str) -> dict:
    """Not a @DBOS.step(): a cheap, idempotent read."""
    return _get(f"{daemon_api_base_url}/v1/policy/approvals/{approval_request_id}", agent_token)


@DBOS.step()
def approve(daemon_api_base_url: str, operator_token: str, approval_request_id: str, resolved_by: str = "") -> dict:
    """Operator-only (see module docstring). Mints and returns a FRESH
    admit PolicyDecision bound to the same operation/action digest as the
    original needs_approval decision — see daemon/policy.Approve's doc
    comment for why this is a new decision, not the original one flipped
    in place. A @DBOS.step() — an operator's approval is itself a durably
    recorded fact."""
    url = f"{daemon_api_base_url}/v1/policy/approvals/{approval_request_id}/approve"
    with tool_call_span("policy:approve", **{"amh.api.url": url}):
        return _post(url, operator_token, {"resolved_by": resolved_by} if resolved_by else {})


@DBOS.step()
def deny(daemon_api_base_url: str, operator_token: str, approval_request_id: str, resolved_by: str = "", reason: str = "") -> dict:
    """Operator-only (see module docstring). Mints no decision — the
    underlying action stays permanently un-admitted. A @DBOS.step() for
    the same durability reason as approve()."""
    url = f"{daemon_api_base_url}/v1/policy/approvals/{approval_request_id}/deny"
    body: dict = {}
    if resolved_by:
        body["resolved_by"] = resolved_by
    if reason:
        body["reason"] = reason
    with tool_call_span("policy:deny", **{"amh.api.url": url}):
        return _post(url, operator_token, body)
