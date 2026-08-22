"""End-to-end tests for the external-effect lifecycle client
(agents/workflows/operations.py over daemon/operations + daemon/api's
/v1/operations/* routes) — driven from Python against a real Go daemon,
mirroring test_policy_e2e.py's pattern.

Requires a working Go toolchain (go build). Skipped if `go` is
unavailable.
"""

from __future__ import annotations

import shutil

import pytest

from workflows.operations import (
    OperationsError,
    get_effect,
    list_effects_by_operation,
    mark_dispatch_pending,
    mark_dispatched,
    mark_observed,
    propose,
    resolve,
)

pytestmark = pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain not available")


def test_propose_verified_reversibility_admits(daemon):
    eff = propose(daemon.base_url, daemon.agent_token, "op-1", "amh.core/test", "test_effect", {"x": 1}, "verified")
    assert eff["state"] == "admitted"
    assert eff["effect_id"]


def test_propose_reversibility_none_needs_approval_and_stays_there(daemon):
    """The track-only posture agentic_loop.py's MCP call site relies on:
    an unattested effect legitimately parks at needs_approval forever,
    since nothing here calls approve()/deny() on it."""
    eff = propose(daemon.base_url, daemon.agent_token, "op-1", "amh.core/mcp-client", "mcp_tool_call:fs:write_file", {"x": 1}, "none")
    assert eff["state"] == "needs_approval"

    fetched = get_effect(daemon.base_url, daemon.agent_token, eff["effect_id"])
    assert fetched["state"] == "needs_approval"


def test_full_happy_path_admitted_through_confirmed(daemon):
    eff = propose(daemon.base_url, daemon.agent_token, "op-2", "amh.core/test", "test_effect", {"x": 1}, "verified")
    effect_id = eff["effect_id"]

    pending = mark_dispatch_pending(daemon.base_url, daemon.agent_token, effect_id)
    assert pending["state"] == "dispatch_pending"

    dispatched = mark_dispatched(daemon.base_url, daemon.agent_token, effect_id, external_command_id="cmd-1")
    assert dispatched["state"] == "dispatched"
    assert dispatched["external_command_id"] == "cmd-1"

    observed = mark_observed(daemon.base_url, daemon.agent_token, effect_id, observation_ref="obs-1")
    assert observed["state"] == "observed"
    assert observed["observation_ref"] == "obs-1"

    resolved = resolve(daemon.base_url, daemon.agent_token, effect_id, "confirmed")
    assert resolved["state"] == "confirmed"


def test_resolve_failed_carries_the_caller_supplied_error(daemon):
    eff = propose(daemon.base_url, daemon.agent_token, "op-3", "amh.core/test", "test_effect", {"x": 1}, "verified")
    effect_id = eff["effect_id"]
    mark_dispatch_pending(daemon.base_url, daemon.agent_token, effect_id)
    mark_dispatched(daemon.base_url, daemon.agent_token, effect_id)
    mark_observed(daemon.base_url, daemon.agent_token, effect_id)

    resolved = resolve(daemon.base_url, daemon.agent_token, effect_id, "failed", error_code="PROVIDER_CALL_FAILED", error_retryable=True, error_message="boom")
    assert resolved["state"] == "failed"
    assert resolved["error_code"] == "PROVIDER_CALL_FAILED"
    assert resolved["error_retryable"] is True
    assert resolved["error_message"] == "boom"


def test_invalid_transition_raises_operations_error(daemon):
    eff = propose(daemon.base_url, daemon.agent_token, "op-4", "amh.core/test", "test_effect", {"x": 1}, "none")
    with pytest.raises(OperationsError):
        # needs_approval -> dispatch_pending requires admitted first.
        mark_dispatch_pending(daemon.base_url, daemon.agent_token, eff["effect_id"])


def test_list_effects_by_operation_round_trips(daemon):
    propose(daemon.base_url, daemon.agent_token, "op-5", "amh.core/test", "test_effect", {"x": 1}, "verified")
    propose(daemon.base_url, daemon.agent_token, "op-5", "amh.core/test", "test_effect_two", {"x": 2}, "verified")

    effects = list_effects_by_operation(daemon.base_url, daemon.agent_token, "op-5")
    assert len(effects) == 2
    assert {e["effect_type"] for e in effects} == {"test_effect", "test_effect_two"}
