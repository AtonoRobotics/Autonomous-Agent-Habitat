"""End-to-end test for the SafetyCase autonomy path (§14.7, Artifact B),
driven entirely from Python against a real Go daemon and a real SSH
device — the standing-autonomy alternative to the ApprovalGate's
per-action ticket (test_approval_e2e.py). Once a case is approved, the
corresponding device action needs no ticket_id to actuate — this test is
what proves that's true end-to-end, not just at the Go layer (already
covered by daemon/api/safetycase_test.go).

Requires a working Go toolchain (go build). Skipped if `go` is
unavailable.
"""

from __future__ import annotations

import json
import shutil

import psycopg
import pytest

from conftest import write_ephemeral_client_key

pytestmark = pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain not available")


def seed_nutrient_doser(db_path: str, host: str, port: int, host_key_authorized_key: str, tmp_path) -> None:
    config = {
        "host": host,
        "port": port,
        "user": "amh",
        "private_key_path": write_ephemeral_client_key(tmp_path),
        "host_key_authorized_key": host_key_authorized_key,
    }
    conn = psycopg.connect(db_path)
    conn.execute(
        "INSERT INTO connector (id, type, auth, config) VALUES ('nutrient-doser-connector', 'ssh', 'none', %s)",
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


def _approve_case_as(daemon, case_id: str, token: str, approved_by: str = "operator:jane"):
    import urllib.error
    import urllib.request

    req = urllib.request.Request(
        f"{daemon.base_url}/v1/safety-cases/{case_id}/approve",
        data=json.dumps({"approved_by": approved_by}).encode(),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}"},
        method="POST",
    )
    try:
        return urllib.request.urlopen(req, timeout=10)
    except urllib.error.HTTPError as e:
        return e


def test_safety_case_grants_standing_autonomy_with_no_ticket(fake_device, daemon, db_path, tmp_path):
    from workflows.actuate import ActuationError, actuate_device
    from workflows.safetycase import create_safety_case, get_status, submit_evidence

    host, port, host_key_authorized_key = fake_device
    seed_nutrient_doser(db_path, host, port, host_key_authorized_key, tmp_path)

    # No case at all: fails closed, same as the no-ApprovalGate-ticket case.
    with pytest.raises(ActuationError):
        actuate_device(daemon.base_url, daemon.agent_token, "nutrient-doser.dispense_ml", {"ml": "5"})

    # Agent opens a case and submits evidence.
    case_id = create_safety_case(
        daemon.base_url, daemon.agent_token,
        subject_id="nutrient-doser.dispense_ml",
        subject_type="device_action",
        risk_class="irreversible_high_consequence",
    )
    assert case_id
    submit_evidence(daemon.base_url, daemon.agent_token, case_id, {"guardrail": "max_daily_dose", "proven": True})

    status = get_status(daemon.base_url, daemon.agent_token, case_id)
    assert status["approved"] is False
    assert status["independent_review"] is False

    # Still no approval: still fails closed, still no ticket needed to try.
    with pytest.raises(ActuationError):
        actuate_device(daemon.base_url, daemon.agent_token, "nutrient-doser.dispense_ml", {"ml": "5"})

    # The agent cannot approve its own case.
    self_approve = _approve_case_as(daemon, case_id, daemon.agent_token, approved_by="not-the-agent-itself")
    assert getattr(self_approve, "status", getattr(self_approve, "code", None)) == 403

    # Only the operator's approval actually grants it.
    operator_approve = _approve_case_as(daemon, case_id, daemon.operator_token)
    assert operator_approve.status == 200

    status = get_status(daemon.base_url, daemon.agent_token, case_id)
    assert status["approved"] is True
    assert status["independent_review"] is True

    # NOW the agent can actuate with NO ticket_id at all — the standing
    # SafetyCase grant is sufficient, distinct from a per-call ticket.
    result = actuate_device(daemon.base_url, daemon.agent_token, "nutrient-doser.dispense_ml", {"ml": "5"})
    assert result == "ok"

    conn = psycopg.connect(db_path)
    inverse, outcome = conn.execute(
        "SELECT inverse_payload, outcome FROM device_effect WHERE device_action_id = %s",
        ("nutrient-doser.dispense_ml",),
    ).fetchone()
    assert outcome == "success"
    assert inverse is None
    conn.close()


def test_workflows_safetycase_has_no_approve_or_revoke_function():
    """Same structural guardrail as approval.py's equivalent test: the
    module offers no way to grant or revoke its own case, and no public
    function accepts anything operator-shaped."""
    import inspect

    import workflows.safetycase as safetycase_module

    public_names = {n for n in dir(safetycase_module) if not n.startswith("_")}
    assert "approve" not in public_names
    assert "revoke" not in public_names
    assert not any("approve" in n.lower() for n in public_names)
    assert not any("revoke" in n.lower() for n in public_names)

    for name in public_names:
        obj = getattr(safetycase_module, name)
        if not inspect.isfunction(obj):
            continue
        params = set(inspect.signature(obj).parameters)
        assert not any("operator" in p.lower() for p in params), (
            f"workflows.safetycase.{name} accepts a parameter that looks like an operator "
            "credential — this module must never be able to hold or send one"
        )
