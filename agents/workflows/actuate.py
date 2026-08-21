"""Device actuation step: calls the Go daemon's persistent actuation API
(daemon/api), which owns reversibility gating, the ApprovalGate, and
connector I/O (§12, §14.6, Artifact F).

Bridge note: this replaces an earlier subprocess-per-call design (spawning
the amh-actuate CLI, which re-dialed SSH from scratch every actuation).
The daemon now runs a persistent HTTP endpoint backed by a real connector
registry (daemon/connectors), so this step is a single HTTP round-trip
against a long-lived process — the architectural property the CLI
approach was always meant to be a stand-in for. Spec fidelity note:
Artifact A names contracts/proto (gRPC) for this bridge; see
daemon/api/api.go's doc comment for why this is JSON-over-HTTP instead.

Uses only the standard library (urllib) — no new HTTP client dependency
for what is, for now, a single low-frequency call per actuation.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request

from dbos import DBOS

from context.observability import tool_call_span


class ActuationError(Exception):
    pass


@DBOS.step()
def actuate_device(
    daemon_api_base_url: str,
    agent_token: str,
    device_action_id: str,
    forward: str,
    read_state: str = "",
    ticket_id: str = "",
) -> str:
    """POSTs to the daemon's /v1/device-actions/{id}/actuate and returns
    its result string. Raises ActuationError (with the daemon's structured
    error message) on any failure — including the fail-closed cases
    enforced by daemon/actuation.Execute (no verified inverse + no
    approved SafetyCase + no approved ticket, surfaced as HTTP 403).

    agent_token authenticates as daemon/authn's agent role — sufficient
    to actuate and to create/check approval tickets, but the daemon
    mechanically refuses this role on the approve endpoint (see
    approval.py's module docstring: there is no approve() function here,
    on purpose, and no token this module holds could use one anyway)."""
    body: dict[str, str] = {"forward": forward}
    if read_state:
        body["read_state"] = read_state
    if ticket_id:
        body["ticket_id"] = ticket_id

    url = f"{daemon_api_base_url}/v1/device-actions/{device_action_id}/actuate"
    request = urllib.request.Request(
        url,
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {agent_token}"},
        method="POST",
    )

    with tool_call_span(f"device_action:{device_action_id}", **{"amh.api.url": url}) as span:
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                payload = json.loads(response.read())
        except urllib.error.HTTPError as e:
            payload = json.loads(e.read())
        except urllib.error.URLError as e:
            span.set_attribute("error.type", "connection_error")
            raise ActuationError(f"could not reach daemon API at {url}: {e}") from e

        if payload.get("error"):
            span.set_attribute("error.type", "actuation_error")
            span.set_attribute("error.message", payload["error"])
            raise ActuationError(payload["error"])
        return payload["result"]
