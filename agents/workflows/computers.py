"""Computer (sandbox) HTTP client: provision and tear down an agent's own
compute instance via the Go daemon's control-plane API
(daemon/api/controlplane.go, daemon/sandbox).

Unlike extensions.py and accounts.py, this module DOES perform mutations
with an agent_token — daemon/api's RBAC table allows agent-or-operator on
both /v1/computers (create) and /v1/computers/{id}/destroy, not operator
only. That's a deliberate, narrower application of the same reversibility
principle §12/v6 already gates device actuation on: a computer's
Create/Destroy pair is always a verified inverse of itself by construction
(daemon/sandbox never records a Create with no corresponding teardown
path), so there is no residue here for the ApprovalGate to cover — unlike
installing an extension (new code, unbounded effects) or authenticating an
account (external identity, a secret), which stay operator-only.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request

from dbos import DBOS

from context.observability import tool_call_span


class ComputerError(Exception):
    pass


@DBOS.step()
def create_computer(daemon_api_base_url: str, agent_token: str, agent_id: str, isolation: str, image: str, resource_limits: dict | None = None) -> dict:
    """Provisions a new computer for agent_id. isolation is 'process' or
    'container'; image is a docker image ref (container) or a shell
    command (process). Returns the computer record (id, status,
    runtime_handle, workdir, ...). A @DBOS.step() — provisioning has a
    real side effect (a running process or container) worth durably
    recording."""
    url = f"{daemon_api_base_url}/v1/computers"
    body = {"agent_id": agent_id, "isolation": isolation, "image": image}
    if resource_limits:
        body["resource_limits"] = resource_limits
    request = urllib.request.Request(
        url,
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {agent_token}"},
        method="POST",
    )
    with tool_call_span("computer:create", **{"amh.api.url": url}):
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                return json.loads(response.read())
        except urllib.error.HTTPError as e:
            payload = json.loads(e.read())
            raise ComputerError(payload.get("error", f"HTTP {e.code}")) from e


@DBOS.step()
def destroy_computer(daemon_api_base_url: str, agent_token: str, computer_id: str, reason: str) -> dict:
    """Tears down a computer — the verified inverse of create_computer. A
    @DBOS.step() for the same durability reason as create_computer."""
    url = f"{daemon_api_base_url}/v1/computers/{computer_id}/destroy"
    request = urllib.request.Request(
        url,
        data=json.dumps({"reason": reason}).encode("utf-8"),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {agent_token}"},
        method="POST",
    )
    with tool_call_span("computer:destroy", **{"amh.api.url": url}):
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                return json.loads(response.read())
        except urllib.error.HTTPError as e:
            payload = json.loads(e.read())
            raise ComputerError(payload.get("error", f"HTTP {e.code}")) from e


def get_computer(daemon_api_base_url: str, agent_token: str, computer_id: str) -> dict:
    """Not a @DBOS.step(): a cheap, idempotent read."""
    url = f"{daemon_api_base_url}/v1/computers/{computer_id}"
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {agent_token}"})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as e:
        payload = json.loads(e.read())
        raise ComputerError(payload.get("error", f"HTTP {e.code}")) from e


def list_computers(daemon_api_base_url: str, agent_token: str, agent_id: str) -> list[dict]:
    """Lists every non-destroyed computer belonging to agent_id. Not a
    @DBOS.step(): a cheap, idempotent read."""
    url = f"{daemon_api_base_url}/v1/computers?agent_id={urllib.parse.quote(agent_id)}"
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {agent_token}"})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as e:
        payload = json.loads(e.read())
        raise ComputerError(payload.get("error", f"HTTP {e.code}")) from e
