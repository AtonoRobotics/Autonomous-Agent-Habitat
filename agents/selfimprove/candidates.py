"""Self-improvement candidate lifecycle HTTP client over daemon/api's
/v1/selfimprove/candidates routes (daemon/selfimprove —
docs/AMH-SPECIFICATION.md §10).

get_promoted_prompt is the first real call site anywhere in this
codebase that reads from a promoted CandidateVersion rather than just
managing its bookkeeping — closing §10's "promotion uses the real
extension lifecycle... a real promoted candidate actually changes what
a live call site does." Promote's own DB-level guarantee (a partial
unique index plus a per-class advisory lock — see daemon/selfimprove's
doc comment) means at most one candidate of a given class can ever be
simultaneously 'promoted', so there is never an ambiguous "which one"
to pick among more than one result.

Scope, stated plainly: CandidateVersion has no field beyond the coarse
candidate_class enum ("prompt", "retrieval_policy", "skill", "module",
"core_code") to distinguish WHICH prompt a "prompt"-class candidate
targets. That's fine with exactly one real prompt call site in this
codebase (workflows/goal.py's decompose_goal) — "the promoted prompt
candidate" and "decompose_goal's prompt" are the same thing today — but
this does not yet generalize to a habitat with more than one
independently-promotable prompt. A real, later gap, not a silent
decision.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request


class SelfImproveError(Exception):
    pass


def _error_message(e: urllib.error.HTTPError) -> str:
    """See workflows/policy.py's _error_message: daemon/authn's
    RequireRole middleware can reject a request before any
    /v1/selfimprove handler runs, via plain http.Error (plain text, not
    JSON)."""
    body = e.read()
    try:
        return json.loads(body).get("error", f"HTTP {e.code}")
    except json.JSONDecodeError:
        return body.decode("utf-8", errors="replace").strip() or f"HTTP {e.code}"


def list_candidates(daemon_api_base_url: str, agent_token: str, candidate_class: str = "", status: str = "") -> list[dict]:
    """Real, agent-accessible read of GET /v1/selfimprove/candidates,
    optionally filtered by candidate_class/status."""
    query = urllib.parse.urlencode({k: v for k, v in {"candidate_class": candidate_class, "status": status}.items() if v})
    url = f"{daemon_api_base_url}/v1/selfimprove/candidates"
    if query:
        url = f"{url}?{query}"
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {agent_token}"})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as e:
        raise SelfImproveError(_error_message(e)) from e


def get_promoted_prompt(daemon_api_base_url: str, agent_token: str, default: str) -> str:
    """Returns the currently promoted "prompt"-class candidate's ref
    (its real content) if one exists, otherwise default — never raises
    for the ordinary "nothing promoted yet" case, since a habitat with
    no self-improvement optimizer running at all (the common case today
    — see daemon/selfimprove's own doc comment on this) must fall back
    to its hardcoded prompt exactly as it always has, not fail the call
    that needs a prompt to proceed."""
    candidates = list_candidates(daemon_api_base_url, agent_token, candidate_class="prompt", status="promoted")
    if not candidates:
        return default
    return candidates[0]["ref"]
