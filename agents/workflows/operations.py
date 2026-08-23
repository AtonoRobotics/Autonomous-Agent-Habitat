"""Generic external-effect-lifecycle gRPC client over daemon/grpcapi's
OperationsService (daemon/operations — docs/AMH-SPECIFICATION.md §4).

Phase 2 of the gRPC migration (see workflows/policy.py's module docstring
for Phase 1 and contracts/proto/operations.proto's header comment for this
phase's scope/rationale): this module used to be an HTTP+JSON client of
daemon/api's /v1/operations/* routes. Its one real production call site
(harness/agentic_loop.py's MCP tool-call tracking, called from
workflows/goal.py's do_subagent_work) now needs a daemon_grpc_addr
alongside the daemon_api_base_url it still needs for daemon/inference
(Phase 4, not yet migrated) — both values thread down the same durable
workflow call graph they already did (agents/workflows/dispatcher.py ->
pursue_goal -> ... -> _propose_mcp_effect), one new parameter, not a new
plumbing mechanism.

Deliberately plain functions, not @DBOS.step()-decorated like
workflows/policy.py's decide()/consume(). policy.py's step decoration was
speculative — written before it had a real call site. This module's real
call site already runs entirely inside one @DBOS.step(): do_subagent_work
calls run_agentic_loop synchronously, which runs the whole loop body to
completion before returning. Nesting a DBOS.step() call inside an
already-executing step isn't a meaningful unit of durability there — it
would just be a plain gRPC call with extra bookkeeping DBOS doesn't apply
mid-step. A future call site invoking this module directly from
workflow-level code (outside any step) is free to wrap these calls in
DBOS.step() at that call site.

Track-only, not enforcing: the built-in policy (daemon/policy's
DefaultPolicyID) admits only Reversibility "verified" — an attested
inverse. A generic third-party MCP tool call has no such attestation (the
harness has no way to verify an arbitrary tool has a reverse action), so
propose() here always passes reversibility "none", which legitimately
resolves to needs_approval and stays there. Callers do not gate tool
execution on the resulting decision — the effect record exists so an
interrupted or failed call is visible and reconcilable (§4, invariant #2),
not to block calls today. Enforcing admission before executing an MCP
call is a real, separate step this module deliberately does not take yet;
see harness/agentic_loop.py's doc comment.
"""

from __future__ import annotations

import json
import threading

import grpc

from context.observability import tool_call_span
from workflows.operationspb import operations_pb2, operations_pb2_grpc


class OperationsError(Exception):
    pass


# See workflows/policy.py's identical _channels/_channel pattern: one
# grpc.Channel per distinct address, reused across calls/threads rather
# than dialed fresh each time.
_channels: dict[str, grpc.Channel] = {}
_channels_lock = threading.Lock()


def _channel(daemon_grpc_addr: str) -> grpc.Channel:
    with _channels_lock:
        channel = _channels.get(daemon_grpc_addr)
        if channel is None:
            channel = grpc.insecure_channel(daemon_grpc_addr)
            _channels[daemon_grpc_addr] = channel
        return channel


def _client(daemon_grpc_addr: str) -> operations_pb2_grpc.OperationsServiceStub:
    return operations_pb2_grpc.OperationsServiceStub(_channel(daemon_grpc_addr))


def _call(daemon_grpc_addr: str, token: str, method_name: str, request, timeout: float = 30.0):
    """See workflows/policy.py's identical _call: wraps any RpcError as an
    OperationsError so callers written against the old HTTP client's
    exception type don't need to change."""
    stub = _client(daemon_grpc_addr)
    method = getattr(stub, method_name)
    metadata = (("authorization", f"Bearer {token}"),)
    try:
        return method(request, metadata=metadata, timeout=timeout)
    except grpc.RpcError as e:
        raise OperationsError(e.details() or e.code().name) from e


def _effect_to_dict(eff: operations_pb2.Effect) -> dict:
    return {
        "effect_id": eff.effect_id,
        "operation_id": eff.operation_id,
        "owner_extension_id": eff.owner_extension_id,
        "effect_type": eff.effect_type,
        "decision_id": eff.decision_id,
        "state": eff.state,
        "forward_digest": eff.forward_digest,
        "retry_class": eff.retry_class,
        "external_command_id": eff.external_command_id,
        "observation_ref": eff.observation_ref,
        "observation_payload": eff.observation_payload,
        "error_code": eff.error_code,
        "error_retryable": eff.error_retryable,
        "error_message": eff.error_message,
        "created_at": eff.created_at,
        "updated_at": eff.updated_at,
    }


def propose(
    daemon_grpc_addr: str,
    agent_token: str,
    operation_id: str,
    owner_extension_id: str,
    effect_type: str,
    payload: dict,
    reversibility: str = "none",
    retry_class: str = "never",
) -> dict:
    """Proposes a new effect record for operation_id. Returns the Effect
    (see contracts/effect-record.schema.json): state "admitted" or
    "needs_approval" per daemon/policy's built-in decision, never "denied"
    for this generic policy (see daemon/policy.Decide's doc comment).

    retry_class is one of "never"/"reconcile_before_retry"/"idempotent"
    (§4's pre-dispatch "retry classification" requirement — daemon/
    operations.Propose refuses a request that omits or misspells it).
    Defaults to "never": this module's one real call site (harness/
    agentic_loop.py's MCP tool tracking) has no idempotency story for an
    arbitrary third-party tool call, matching its already-honest
    reversibility="none" default above."""
    req = operations_pb2.ProposeRequest(
        operation_id=operation_id, owner_extension_id=owner_extension_id, effect_type=effect_type,
        payload_json=json.dumps(payload), reversibility=reversibility, retry_class=retry_class,
    )
    with tool_call_span("operations:propose", **{"amh.grpc.addr": daemon_grpc_addr}):
        return _effect_to_dict(_call(daemon_grpc_addr, agent_token, "Propose", req))


def mark_dispatch_pending(daemon_grpc_addr: str, agent_token: str, effect_id: str, payload: dict) -> dict:
    """payload must be the exact payload about to be dispatched — the
    daemon hashes it fresh and binds dispatch to that digest (§15
    acceptance invariant #6: "policy dispatch is bound to the admitted
    action digest and fails closed after expiry or mutation"). Passing
    anything other than the payload propose() actually admitted raises
    OperationsError (FAILED_PRECONDITION)."""
    req = operations_pb2.MarkDispatchPendingRequest(effect_id=effect_id, payload_json=json.dumps(payload))
    with tool_call_span("operations:dispatch_pending", **{"amh.grpc.addr": daemon_grpc_addr}):
        return _effect_to_dict(_call(daemon_grpc_addr, agent_token, "MarkDispatchPending", req))


def mark_dispatched(daemon_grpc_addr: str, agent_token: str, effect_id: str, external_command_id: str = "") -> dict:
    req = operations_pb2.MarkDispatchedRequest(effect_id=effect_id, external_command_id=external_command_id)
    with tool_call_span("operations:dispatched", **{"amh.grpc.addr": daemon_grpc_addr}):
        return _effect_to_dict(_call(daemon_grpc_addr, agent_token, "MarkDispatched", req))


def mark_observed(daemon_grpc_addr: str, agent_token: str, effect_id: str, observation_ref: str = "") -> dict:
    req = operations_pb2.MarkObservedRequest(effect_id=effect_id, observation_ref=observation_ref)
    with tool_call_span("operations:observed", **{"amh.grpc.addr": daemon_grpc_addr}):
        return _effect_to_dict(_call(daemon_grpc_addr, agent_token, "MarkObserved", req))


def resolve(
    daemon_grpc_addr: str,
    agent_token: str,
    effect_id: str,
    terminal: str,
    error_code: str = "",
    error_retryable: bool = False,
    error_message: str = "",
) -> dict:
    """terminal is one of "confirmed"/"reconciled"/"compensated"/"failed"
    — the caller's own verdict, trusted as-is by daemon/operations.Resolve
    for external-effect outcomes specifically (docs/AMH-SPECIFICATION.md
    §4: the core "SHALL NOT infer that [an external] effect failed"), the
    deliberate exception to this codebase's usual never-trust-the-caller
    pattern (contrast daemon/selfimprove's server-computed eval verdict)."""
    req = operations_pb2.ResolveRequest(
        effect_id=effect_id, terminal=terminal,
        error_code=error_code, retryable=error_retryable, message=error_message,
    )
    with tool_call_span("operations:resolve", **{"amh.grpc.addr": daemon_grpc_addr}):
        return _effect_to_dict(_call(daemon_grpc_addr, agent_token, "Resolve", req))


def get_effect(daemon_grpc_addr: str, agent_token: str, effect_id: str) -> dict:
    """A cheap, idempotent read."""
    req = operations_pb2.GetEffectRequest(effect_id=effect_id)
    return _effect_to_dict(_call(daemon_grpc_addr, agent_token, "GetEffect", req))


def list_effects_by_operation(daemon_grpc_addr: str, agent_token: str, operation_id: str) -> list[dict]:
    """A cheap, idempotent read of every effect proposed under operation_id."""
    req = operations_pb2.ListEffectsByOperationRequest(operation_id=operation_id)
    resp = _call(daemon_grpc_addr, agent_token, "ListEffectsByOperation", req)
    return [_effect_to_dict(eff) for eff in resp.effects]
