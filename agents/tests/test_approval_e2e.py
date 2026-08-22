"""End-to-end test for the irreversible-action / ApprovalGate path
(§12, §14.7, Artifact B), driven entirely from Python against a real Go
daemon and a real SSH device — the residue §12/v6 scopes the ApprovalGate
to: an action with no verified inverse and no approved SafetyCase.

Complements test_greenhouse_e2e.py, which only exercises the autonomous
(verified-inverse) path; this test is what proves the fail-closed gate,
its HTTP-driven approval flow, and the agent/operator token split (daemon
/authn) actually work end-to-end from the Python side — not just at the
Go layer (already covered by daemon/api/approval_test.go).

Requires a working Go toolchain (go build). Skipped if `go` is
unavailable.
"""

from __future__ import annotations

import json
import shutil
import sqlite3

import pytest

from conftest import write_ephemeral_client_key

pytestmark = pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain not available")


def seed_nutrient_doser(db_path: str, host: str, port: int, host_key_authorized_key: str, tmp_path) -> None:
    """An irreversible action (no inverse_template, no verified_at) —
    dispensing a nutrient is a consuming action with no undo endpoint,
    per contracts/manifests/connector.manifest.yaml's own worked example."""
    config = {
        "host": host,
        "port": port,
        "user": "amh",
        "private_key_path": write_ephemeral_client_key(tmp_path),
        "host_key_authorized_key": host_key_authorized_key,
    }
    conn = sqlite3.connect(db_path)
    conn.execute(
        "INSERT INTO connector (id, type, auth, config) VALUES ('nutrient-doser-connector', 'ssh', 'none', ?)",
        (json.dumps(config),),
    )
    conn.execute(
        "INSERT INTO device (id, kind, connector_id) VALUES ('nutrient-doser', 'doser', 'nutrient-doser-connector')"
    )
    conn.execute(
        """INSERT INTO device_action (id, device_id, name, reversible, forward_template)
           VALUES ('nutrient-doser.dispense_ml', 'nutrient-doser', 'dispense_ml', 0,
                   '{"shell_template": "dose {{ml}}ml"}')"""
    )
    conn.commit()
    conn.close()


def _approve_as(daemon, ticket_id: str, token: str, approved_by: str = "operator:jane"):
    """Simulates an operator hitting the daemon's approve endpoint
    directly with a given bearer token. Deliberately not routed through
    workflows.approval, which has no approve() function to call at all —
    see that module's docstring and test_workflows_approval_has_no_*
    below."""
    import urllib.error
    import urllib.request

    req = urllib.request.Request(
        f"{daemon.base_url}/v1/approval-gates/{ticket_id}/approve",
        data=json.dumps({"approved_by": approved_by}).encode(),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}"},
        method="POST",
    )
    try:
        return urllib.request.urlopen(req, timeout=10)
    except urllib.error.HTTPError as e:
        return e  # HTTPError is also a valid response-like object (has .status/.code)


def test_irreversible_action_requires_approval_over_http(fake_device, daemon, db_path, tmp_path):
    from workflows.actuate import ActuationError, actuate_device
    from workflows.approval import is_approved, request_approval

    host, port, host_key_authorized_key = fake_device
    seed_nutrient_doser(db_path, host, port, host_key_authorized_key, tmp_path)

    params = {"ml": "5"}

    # No ticket at all: fails closed with the daemon's fail-closed error.
    with pytest.raises(ActuationError):
        actuate_device(daemon.base_url, daemon.agent_token, "nutrient-doser.dispense_ml", params)

    # Request approval — a real ticket, created via the daemon's API,
    # using only the agent token, bound to this exact action.
    ticket_id = request_approval(
        daemon.base_url,
        daemon.agent_token,
        device_action_id="nutrient-doser.dispense_ml",
        params=params,
        risk="irreversible",
        reason="scheduled feeding",
    )
    assert ticket_id

    # Unapproved ticket: still fails closed.
    assert is_approved(daemon.base_url, daemon.agent_token, ticket_id) is False
    with pytest.raises(ActuationError):
        actuate_device(daemon.base_url, daemon.agent_token, "nutrient-doser.dispense_ml", params, ticket_id=ticket_id)

    # The agent CANNOT approve its own ticket — the daemon refuses the
    # agent token on this endpoint regardless of what approved_by claims.
    self_approve = _approve_as(daemon, ticket_id, daemon.agent_token, approved_by="totally-not-the-agent-itself")
    assert getattr(self_approve, "status", getattr(self_approve, "code", None)) == 403
    assert is_approved(daemon.base_url, daemon.agent_token, ticket_id) is False

    # Only the operator token actually approves it.
    operator_approve = _approve_as(daemon, ticket_id, daemon.operator_token)
    assert operator_approve.status == 200

    # Now it proceeds.
    assert is_approved(daemon.base_url, daemon.agent_token, ticket_id) is True
    result = actuate_device(daemon.base_url, daemon.agent_token, "nutrient-doser.dispense_ml", params, ticket_id=ticket_id)
    assert result == "ok"

    # An irreversible action's effect must record no inverse — nothing to
    # auto-reverse, by construction.
    conn = sqlite3.connect(db_path)
    inverse, outcome = conn.execute(
        "SELECT inverse_payload, outcome FROM device_effect WHERE device_action_id = ?",
        ("nutrient-doser.dispense_ml",),
    ).fetchone()
    assert outcome == "success"
    assert inverse is None
    conn.close()


def test_workflows_approval_has_no_self_approve_function():
    """Structural guardrail, not a runtime check: the ApprovalGate's whole
    point is that approval is agent-external (§14.7's anti-reward-hacking
    discipline — the same DGM cautionary case §10 cites). This asserts the
    Python client module offers no function that could let a workflow
    grant its own request, AND that none of its public functions even
    accept an operator-token-shaped parameter — the module should be
    structurally incapable of holding that credential, not just
    conventionally discouraged from using it."""
    import inspect

    import workflows.approval as approval_module

    public_names = {n for n in dir(approval_module) if not n.startswith("_")}
    assert "approve" not in public_names
    assert not any("approve" in n.lower() and "is_approved" not in n.lower() for n in public_names)

    for name in public_names:
        obj = getattr(approval_module, name)
        if not inspect.isfunction(obj):
            continue
        params = set(inspect.signature(obj).parameters)
        assert not any("operator" in p.lower() for p in params), (
            f"workflows.approval.{name} accepts a parameter that looks like an operator "
            "credential — this module must never be able to hold or send one"
        )
