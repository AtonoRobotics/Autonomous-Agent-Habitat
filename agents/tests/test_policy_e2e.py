"""End-to-end tests for the generic policy/approval seam
(agents/workflows/policy.py over daemon/policy + daemon/api's /v1/policy/*
routes) — driven from Python against a real Go daemon, proving the seam
works end to end, not just at the Go layer (already covered by
daemon/policy's and daemon/api's own tests).

Requires a working Go toolchain (go build). Skipped if `go` is
unavailable.
"""

from __future__ import annotations

import shutil

import pytest

from workflows.policy import (
    PolicyError,
    approve,
    consume,
    decide,
    deny,
    get_approval_request,
    get_decision,
    list_pending_approvals,
)

pytestmark = pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain not available")


def test_decide_verified_reversibility_admits_then_consume_succeeds(daemon):
    payload = {"open_pct": 60}
    d = decide(daemon.base_url, daemon.agent_token, "op-1", payload, "verified")
    assert d["result"] == "admit"
    assert not d.get("approval_request_id")

    consume(daemon.base_url, daemon.agent_token, d["id"], payload)

    with pytest.raises(PolicyError):
        consume(daemon.base_url, daemon.agent_token, d["id"], payload)


def test_consume_different_payload_than_decided_fails(daemon):
    d = decide(daemon.base_url, daemon.agent_token, "op-1", {"open_pct": 60}, "verified")
    with pytest.raises(PolicyError):
        consume(daemon.base_url, daemon.agent_token, d["id"], {"open_pct": 99})


def test_get_decision_round_trips(daemon):
    d = decide(daemon.base_url, daemon.agent_token, "op-1", {"x": 1}, "verified")
    fetched = get_decision(daemon.base_url, daemon.agent_token, d["id"])
    assert fetched["id"] == d["id"]
    assert fetched["result"] == "admit"


def test_needs_approval_loop_agent_cannot_self_approve_operator_can(daemon):
    payload = {"ml": 5}
    d = decide(daemon.base_url, daemon.agent_token, "op-1", payload, "none")
    assert d["result"] == "needs_approval"
    assert d["approval_request_id"]

    # Not yet consumable.
    with pytest.raises(PolicyError):
        consume(daemon.base_url, daemon.agent_token, d["id"], payload)

    # An agent token cannot approve its own request — the daemon
    # mechanically refuses this (403), not this client.
    with pytest.raises(PolicyError):
        approve(daemon.base_url, daemon.agent_token, d["approval_request_id"])

    pending = list_pending_approvals(daemon.base_url, daemon.agent_token)
    assert any(a["id"] == d["approval_request_id"] for a in pending)

    approved = approve(daemon.base_url, daemon.operator_token, d["approval_request_id"], resolved_by="operator:jane")
    assert approved["result"] == "admit"
    assert approved["id"] != d["id"]  # a fresh decision, not the original mutated

    # The original decision is untouched.
    original = get_decision(daemon.base_url, daemon.agent_token, d["id"])
    assert original["result"] == "needs_approval"

    # Now the freshly-minted decision is consumable.
    consume(daemon.base_url, daemon.agent_token, approved["id"], payload)

    pending_after = list_pending_approvals(daemon.base_url, daemon.agent_token)
    assert not any(a["id"] == d["approval_request_id"] for a in pending_after)


def test_deny_leaves_action_permanently_unadmitted(daemon):
    payload = {"x": 1}
    d = decide(daemon.base_url, daemon.agent_token, "op-1", payload, "claimed")

    denied = deny(daemon.base_url, daemon.operator_token, d["approval_request_id"], resolved_by="operator:jane", reason="too risky")
    assert denied["status"] == "denied"
    assert denied["reason"] == "too risky"

    with pytest.raises(PolicyError):
        consume(daemon.base_url, daemon.agent_token, d["id"], payload)


def test_get_approval_request_round_trips(daemon):
    d = decide(daemon.base_url, daemon.agent_token, "op-1", {"x": 1}, "none")
    ar = get_approval_request(daemon.base_url, daemon.agent_token, d["approval_request_id"])
    assert ar["id"] == d["approval_request_id"]
    assert ar["status"] == "pending"
