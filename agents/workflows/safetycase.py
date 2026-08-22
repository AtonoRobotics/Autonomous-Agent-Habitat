"""SafetyCase HTTP client: the harder evidence path to earned autonomy
for actions with no verified inverse (§14.7, Artifact B/C/E). Where
approval.py's ApprovalGate gates one action at a time, a SafetyCase is a
standing, revocable grant of autonomy for a whole device_action or
capability — once approved, the corresponding actuate_device call no
longer needs a ticket_id at all (see daemon/actuation's
hasApprovedSafetyCase and test_safetycase_e2e.py).

Same security posture as approval.py, restated here rather than assumed:
every function takes only an agent_token. This module has no function
named approve or revoke, and none that sends an operator token — the
daemon mechanically refuses an agent token on those endpoints (403)
regardless of what this module does, but the module's own shape is a
second, independent line of defense against a compromised or malicious
workflow granting itself standing autonomy over an irreversible action.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request

from dbos import DBOS

from context.observability import tool_call_span


class SafetyCaseError(Exception):
    pass


@DBOS.step()
def create_safety_case(daemon_api_base_url: str, agent_token: str, subject_id: str, subject_type: str, risk_class: str) -> str:
    """Opens a new SafetyCase and returns its ID. subject_type must be
    'device_action' or 'capability'; risk_class must be one of 'low',
    'moderate', 'high', 'irreversible_high_consequence' (mirrors
    safetycase.SubjectType/RiskClass on the Go side). A @DBOS.step() so a
    crash between "case created" and "workflow recorded the ID" cannot
    lose track of it."""
    url = f"{daemon_api_base_url}/v1/safety-cases"
    request = urllib.request.Request(
        url,
        data=json.dumps({"subject_id": subject_id, "subject_type": subject_type, "risk_class": risk_class}).encode("utf-8"),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {agent_token}"},
        method="POST",
    )
    with tool_call_span("safety_case:create", **{"amh.api.url": url}):
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                payload = json.loads(response.read())
        except urllib.error.HTTPError as e:
            payload = json.loads(e.read())
            raise SafetyCaseError(payload.get("error", f"HTTP {e.code}")) from e
        return payload["case_id"]


@DBOS.step()
def submit_evidence(daemon_api_base_url: str, agent_token: str, case_id: str, guardrail_proof: dict) -> None:
    """Appends one guardrail-proof entry to the case's accumulated
    evidence. A @DBOS.step() — evidence submission is itself part of the
    case's audit trail and worth durably recording, unlike a status
    poll."""
    url = f"{daemon_api_base_url}/v1/safety-cases/{case_id}/evidence"
    request = urllib.request.Request(
        url,
        data=json.dumps({"guardrail_proof": guardrail_proof}).encode("utf-8"),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {agent_token}"},
        method="POST",
    )
    with tool_call_span("safety_case:submit_evidence", **{"amh.api.url": url}):
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                json.loads(response.read())
        except urllib.error.HTTPError as e:
            payload = json.loads(e.read())
            raise SafetyCaseError(payload.get("error", f"HTTP {e.code}")) from e


def get_status(daemon_api_base_url: str, agent_token: str, case_id: str) -> dict:
    """Checks a case's current state. Not a @DBOS.step(): a cheap,
    idempotent read — the actuation step that later relies on this case
    being approved is what needs durability, not each status check."""
    url = f"{daemon_api_base_url}/v1/safety-cases/{case_id}"
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {agent_token}"})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as e:
        payload = json.loads(e.read())
        raise SafetyCaseError(payload.get("error", f"HTTP {e.code}")) from e
