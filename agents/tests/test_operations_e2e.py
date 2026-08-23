"""End-to-end tests for the external-effect lifecycle client
(agents/workflows/operations.py over daemon/operations + daemon/grpcapi's
OperationsService) — driven from Python against a real Go daemon over a
real gRPC connection, mirroring test_policy_e2e.py's pattern.

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
    eff = propose(daemon.grpc_addr, daemon.agent_token, "op-1", "amh.core/test", "test_effect", {"x": 1}, "verified")
    assert eff["state"] == "admitted"
    assert eff["effect_id"]


def test_propose_defaults_retry_class_to_never(daemon):
    """§4's pre-dispatch 'retry classification' requirement — propose()'s
    default matches its one real caller's honest posture (harness/
    agentic_loop.py's MCP call site has no idempotency story for an
    arbitrary third-party tool call)."""
    eff = propose(daemon.grpc_addr, daemon.agent_token, "op-1", "amh.core/test", "test_effect", {"x": 1}, "verified")
    assert eff["retry_class"] == "never"


def test_propose_rejects_an_invalid_retry_class(daemon):
    with pytest.raises(OperationsError):
        propose(daemon.grpc_addr, daemon.agent_token, "op-1", "amh.core/test", "test_effect", {"x": 1}, "verified", "whenever-i-feel-like-it")


def test_propose_reversibility_none_needs_approval_and_stays_there(daemon):
    """The track-only posture agentic_loop.py's MCP call site relies on:
    an unattested effect legitimately parks at needs_approval forever,
    since nothing here calls approve()/deny() on it."""
    eff = propose(daemon.grpc_addr, daemon.agent_token, "op-1", "amh.core/mcp-client", "mcp_tool_call:fs:write_file", {"x": 1}, "none")
    assert eff["state"] == "needs_approval"

    fetched = get_effect(daemon.grpc_addr, daemon.agent_token, eff["effect_id"])
    assert fetched["state"] == "needs_approval"


def test_full_happy_path_admitted_through_confirmed(daemon):
    eff = propose(daemon.grpc_addr, daemon.agent_token, "op-2", "amh.core/test", "test_effect", {"x": 1}, "verified")
    effect_id = eff["effect_id"]

    pending = mark_dispatch_pending(daemon.grpc_addr, daemon.agent_token, effect_id, {"x": 1})
    assert pending["state"] == "dispatch_pending"

    dispatched = mark_dispatched(daemon.grpc_addr, daemon.agent_token, effect_id, external_command_id="cmd-1")
    assert dispatched["state"] == "dispatched"
    assert dispatched["external_command_id"] == "cmd-1"

    observed = mark_observed(daemon.grpc_addr, daemon.agent_token, effect_id, observation_ref="obs-1")
    assert observed["state"] == "observed"
    assert observed["observation_ref"] == "obs-1"

    resolved = resolve(daemon.grpc_addr, daemon.agent_token, effect_id, "confirmed")
    assert resolved["state"] == "confirmed"


def test_resolve_failed_carries_the_caller_supplied_error(daemon):
    eff = propose(daemon.grpc_addr, daemon.agent_token, "op-3", "amh.core/test", "test_effect", {"x": 1}, "verified")
    effect_id = eff["effect_id"]
    mark_dispatch_pending(daemon.grpc_addr, daemon.agent_token, effect_id, {"x": 1})
    mark_dispatched(daemon.grpc_addr, daemon.agent_token, effect_id)
    mark_observed(daemon.grpc_addr, daemon.agent_token, effect_id)

    resolved = resolve(daemon.grpc_addr, daemon.agent_token, effect_id, "failed", error_code="PROVIDER_CALL_FAILED", error_retryable=True, error_message="boom")
    assert resolved["state"] == "failed"
    assert resolved["error_code"] == "PROVIDER_CALL_FAILED"
    assert resolved["error_retryable"] is True
    assert resolved["error_message"] == "boom"


def test_invalid_transition_raises_operations_error(daemon):
    eff = propose(daemon.grpc_addr, daemon.agent_token, "op-4", "amh.core/test", "test_effect", {"x": 1}, "none")
    with pytest.raises(OperationsError):
        # needs_approval -> dispatch_pending requires admitted first.
        mark_dispatch_pending(daemon.grpc_addr, daemon.agent_token, eff["effect_id"], {"x": 1})


def test_mark_dispatch_pending_with_mutated_payload_fails_closed(daemon):
    """§15 acceptance invariant #6: dispatch is bound to the digest of
    the payload actually admitted — a caller presenting a different
    payload at dispatch time must be rejected, not silently waved
    through, and the effect must stay admitted rather than advance."""
    eff = propose(daemon.grpc_addr, daemon.agent_token, "op-6", "amh.core/test", "test_effect", {"x": 1}, "verified")
    effect_id = eff["effect_id"]

    with pytest.raises(OperationsError):
        mark_dispatch_pending(daemon.grpc_addr, daemon.agent_token, effect_id, {"x": 999})

    fetched = get_effect(daemon.grpc_addr, daemon.agent_token, effect_id)
    assert fetched["state"] == "admitted"

    pending = mark_dispatch_pending(daemon.grpc_addr, daemon.agent_token, effect_id, {"x": 1})
    assert pending["state"] == "dispatch_pending"


def test_list_effects_by_operation_round_trips(daemon):
    propose(daemon.grpc_addr, daemon.agent_token, "op-5", "amh.core/test", "test_effect", {"x": 1}, "verified")
    propose(daemon.grpc_addr, daemon.agent_token, "op-5", "amh.core/test", "test_effect_two", {"x": 2}, "verified")

    effects = list_effects_by_operation(daemon.grpc_addr, daemon.agent_token, "op-5")
    assert len(effects) == 2
    assert {e["effect_type"] for e in effects} == {"test_effect", "test_effect_two"}
