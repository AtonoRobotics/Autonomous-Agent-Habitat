"""ApprovalGate HTTP client: request and check approval tickets against
the Go daemon's persistent API (daemon/api), for actions with no verified
inverse and no approved SafetyCase — the residue §12/v6 scopes the
ApprovalGate to. See docs/AMH-SPECIFICATION.md Artifact B (ApprovalGate
protocol).

Creating a ticket never grants anything by itself — approval is always
out-of-band (an operator today; a defined independent-reviewer role for a
SafetyCase per §14.7). request_approval is a @DBOS.step() so a crash
between "ticket created" and "workflow recorded the ticket ID" cannot
lose track of a pending approval; wait_for_approval is deliberately NOT a
step (see its docstring) since it polls and blocking a DBOS step on human
timescales is the wrong shape for this durability layer.

Every function here takes an agent_token, authenticating as
daemon/authn's agent role. This module has no function that sends an
operator token, and no function named approve — that is not an oversight
to fix later, it is the point: the daemon mechanically refuses an agent
token on the approve endpoint (403), so even a compromised or malicious
workflow calling every public function in this module cannot grant its
own request. Approval requires the separate operator token, which this
module never touches.
"""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request

from dbos import DBOS

from context.observability import tool_call_span


class ApprovalError(Exception):
    pass


@DBOS.step()
def request_approval(daemon_api_base_url: str, agent_token: str, action: dict, risk: str) -> str:
    """Creates an approval_gate ticket and returns its ID. risk must be
    'reversible' or 'irreversible' (mirrors interlocks.Risk on the Go
    side)."""
    if risk not in ("reversible", "irreversible"):
        raise ValueError(f"risk must be 'reversible' or 'irreversible', got {risk!r}")

    url = f"{daemon_api_base_url}/v1/approval-gates"
    request = urllib.request.Request(
        url,
        data=json.dumps({"action": action, "risk": risk}).encode("utf-8"),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {agent_token}"},
        method="POST",
    )
    with tool_call_span("approval_gate:create", **{"amh.api.url": url}):
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                payload = json.loads(response.read())
        except urllib.error.HTTPError as e:
            payload = json.loads(e.read())
            raise ApprovalError(payload.get("error", f"HTTP {e.code}")) from e
        return payload["ticket_id"]


def is_approved(daemon_api_base_url: str, agent_token: str, ticket_id: str) -> bool:
    """Checks a ticket's current status. Not a @DBOS.step(): this is a
    cheap, idempotent read with no side effects worth durably recording on
    its own — the actuation step that later enforces the ticket is what
    needs durability, not each poll."""
    url = f"{daemon_api_base_url}/v1/approval-gates/{ticket_id}"
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {agent_token}"})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            payload = json.loads(response.read())
    except urllib.error.HTTPError as e:
        payload = json.loads(e.read())
        raise ApprovalError(payload.get("error", f"HTTP {e.code}")) from e
    return bool(payload["satisfied"])


def wait_for_approval(daemon_api_base_url: str, agent_token: str, ticket_id: str, timeout_s: float = 300, poll_interval_s: float = 2) -> None:
    """Blocks the calling (non-workflow) code until a ticket is approved
    or timeout_s elapses. Deliberately NOT a @DBOS.step(): a step that
    blocks for up to five minutes ties up a DBOS worker thread for
    something that should instead be driven by an external event (a
    human's action) — the earned-autonomy / SafetyCase design in §14.7
    envisions review as an asynchronous, agent-external act, not something
    the durability layer waits on synchronously. This polls the daemon API
    rather than using DBOS.recv/DBOS.send (the mechanism the spec's
    ApprovalGate design note in Artifact B names) — a caller with many
    concurrent pending approvals should replace this with recv/send
    instead of scaling up poll frequency.
    """
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        if is_approved(daemon_api_base_url, agent_token, ticket_id):
            return
        time.sleep(poll_interval_s)
    raise TimeoutError(f"ticket {ticket_id} not approved within {timeout_s}s")
