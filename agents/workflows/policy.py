"""Generic policy/approval gRPC client over daemon/grpcapi's PolicyService
(daemon/policy — docs/AMH-SPECIFICATION.md §6).

Phase 1 of the gRPC migration (docs/AMH-SPECIFICATION.md §3.1/§3.3: "local
gRPC transport" / "local gRPC for synchronous daemon/worker calls"): this
module used to be an HTTP+JSON client of daemon/api's /v1/policy/* routes.
policy has no browser (control-plane UI) caller and no external-protocol
caller — see contracts/proto/policy.proto's header comment for why it was
the first service moved.

decide()/consume() are agent-token: proposing an action and, once
admitted, dispatching it are exactly the "agents propose" half of
decision 9 that daemon/grpcapi's RBAC table allows an agent token to do.
approve()/deny() take an operator_token instead — the daemon mechanically
refuses an agent token on those RPCs (PermissionDenied), the same
anti-self-approval property every other operator-only route enforces. A
workflow that reaches needs_approval cannot resolve its own request; it
has to wait for an operator to call approve()/deny() out-of-band (e.g.
from the control-plane UI) and can poll get_decision()/
get_approval_request() to learn the outcome.
"""

from __future__ import annotations

import json
import threading

import grpc
from dbos import DBOS

from context.observability import tool_call_span
from workflows.policypb import policy_pb2, policy_pb2_grpc


class PolicyError(Exception):
    pass


# grpc.Channel is safe to share across calls/threads (it multiplexes
# streams over one HTTP/2 connection internally), so one channel per
# distinct daemon_grpc_addr is reused rather than dialed fresh on every
# call. Keyed by address alone, not by token: the channel is transport,
# not identity — the bearer token still travels per-call in _client below,
# the same way agent vs. operator identity was per-request, not
# per-connection, over the old HTTP client.
_channels: dict[str, grpc.Channel] = {}
_channels_lock = threading.Lock()


def _channel(daemon_grpc_addr: str) -> grpc.Channel:
    with _channels_lock:
        channel = _channels.get(daemon_grpc_addr)
        if channel is None:
            channel = grpc.insecure_channel(daemon_grpc_addr)
            _channels[daemon_grpc_addr] = channel
        return channel


def _client(daemon_grpc_addr: str) -> policy_pb2_grpc.PolicyServiceStub:
    return policy_pb2_grpc.PolicyServiceStub(_channel(daemon_grpc_addr))


def _call(daemon_grpc_addr: str, token: str, method_name: str, request, timeout: float = 30.0):
    """Invokes one PolicyService RPC with token as the bearer credential —
    the gRPC analog of the old HTTP client's Authorization header. Wraps
    any RpcError as a PolicyError so callers written against the old HTTP
    client's exception type don't need to change: NOT_FOUND/
    FAILED_PRECONDITION/INVALID_ARGUMENT/UNAUTHENTICATED/PERMISSION_DENIED
    all become PolicyError, the same "any non-2xx is a PolicyError"
    contract _post/_get enforced over HTTP."""
    stub = _client(daemon_grpc_addr)
    method = getattr(stub, method_name)
    metadata = (("authorization", f"Bearer {token}"),)
    try:
        return method(request, metadata=metadata, timeout=timeout)
    except grpc.RpcError as e:
        raise PolicyError(e.details() or e.code().name) from e


def _decision_to_dict(d: policy_pb2.Decision) -> dict:
    return {
        "id": d.id,
        "operation_id": d.operation_id,
        "action_digest": d.action_digest,
        "policy_id": d.policy_id,
        "policy_version": d.policy_version,
        "result": d.result,
        "reason_codes": list(d.reason_codes),
        "approval_request_id": d.approval_request_id,
        "decided_at": d.decided_at,
        "expires_at": d.expires_at,
        "consumed_at": d.consumed_at,
    }


def _approval_to_dict(a: policy_pb2.ApprovalRequest) -> dict:
    return {
        "id": a.id,
        "decision_id": a.decision_id,
        "status": a.status,
        "resolved_by": a.resolved_by,
        "resolved_at": a.resolved_at,
        "reason": a.reason,
    }


@DBOS.step()
def decide(daemon_grpc_addr: str, agent_token: str, operation_id: str, payload: dict, reversibility: str) -> dict:
    """Proposes operation_id/payload to the policy hook. reversibility is
    "verified", "claimed", or "none" (contracts/action-envelope.schema.json's
    properties.reversibility.status) — the one generic property
    daemon/policy's built-in policy evaluates. Returns the PolicyDecision:
    result "admit"/"admit_with_constraints" means consume() may proceed;
    "needs_approval" means wait on approval_request_id via
    get_approval_request() or get_decision(); "deny"/"defer" mean stop. A
    @DBOS.step() — the decision itself is a durably recorded fact, not a
    cheap read."""
    req = policy_pb2.DecideRequest(operation_id=operation_id, payload_json=json.dumps(payload), reversibility=reversibility)
    with tool_call_span("policy:decide", **{"amh.grpc.addr": daemon_grpc_addr}):
        return _decision_to_dict(_call(daemon_grpc_addr, agent_token, "Decide", req))


@DBOS.step()
def consume(daemon_grpc_addr: str, agent_token: str, decision_id: str, payload: dict) -> None:
    """Atomically single-uses decision_id right before dispatching payload
    — payload MUST be byte-for-byte the same value passed to decide(); the
    daemon recomputes its digest and refuses to consume a decision bound
    to a different payload (policy.ErrDigestMismatch), not just a
    different decision_id. Raises PolicyError on any failure: already
    consumed, expired, not admitted, or digest mismatch — every one of
    those means "do not dispatch," which is why this has no return value
    for callers to ignore."""
    req = policy_pb2.ConsumeRequest(decision_id=decision_id, payload_json=json.dumps(payload))
    with tool_call_span("policy:consume", **{"amh.grpc.addr": daemon_grpc_addr}):
        _call(daemon_grpc_addr, agent_token, "Consume", req)


def get_decision(daemon_grpc_addr: str, agent_token: str, decision_id: str) -> dict:
    """Not a @DBOS.step(): a cheap, idempotent read — for a workflow
    polling a needs_approval decision's fate, or checking whether a
    decision it already holds has since been consumed."""
    return _decision_to_dict(_call(daemon_grpc_addr, agent_token, "GetDecision", policy_pb2.GetDecisionRequest(id=decision_id)))


def list_pending_approvals(daemon_grpc_addr: str, agent_token: str) -> list[dict]:
    """Not a @DBOS.step(): a cheap, idempotent read of every ApprovalRequest
    still awaiting operator resolution — the queue an operator surface
    polls or lists."""
    resp = _call(daemon_grpc_addr, agent_token, "ListPendingApprovals", policy_pb2.ListPendingApprovalsRequest())
    return [_approval_to_dict(a) for a in resp.approvals]


def get_approval_request(daemon_grpc_addr: str, agent_token: str, approval_request_id: str) -> dict:
    """Not a @DBOS.step(): a cheap, idempotent read."""
    req = policy_pb2.GetApprovalRequestRequest(id=approval_request_id)
    return _approval_to_dict(_call(daemon_grpc_addr, agent_token, "GetApprovalRequest", req))


@DBOS.step()
def approve(daemon_grpc_addr: str, operator_token: str, approval_request_id: str, resolved_by: str = "") -> dict:
    """Operator-only (see module docstring). Mints and returns a FRESH
    admit PolicyDecision bound to the same operation/action digest as the
    original needs_approval decision — see daemon/policy.Approve's doc
    comment for why this is a new decision, not the original one flipped
    in place. A @DBOS.step() — an operator's approval is itself a durably
    recorded fact."""
    req = policy_pb2.ResolveApprovalRequest(approval_id=approval_request_id, resolved_by=resolved_by)
    with tool_call_span("policy:approve", **{"amh.grpc.addr": daemon_grpc_addr}):
        return _decision_to_dict(_call(daemon_grpc_addr, operator_token, "Approve", req))


@DBOS.step()
def deny(daemon_grpc_addr: str, operator_token: str, approval_request_id: str, resolved_by: str = "", reason: str = "") -> dict:
    """Operator-only (see module docstring). Mints no decision — the
    underlying action stays permanently un-admitted. A @DBOS.step() for
    the same durability reason as approve()."""
    req = policy_pb2.ResolveApprovalRequest(approval_id=approval_request_id, resolved_by=resolved_by, reason=reason)
    with tool_call_span("policy:deny", **{"amh.grpc.addr": daemon_grpc_addr}):
        return _approval_to_dict(_call(daemon_grpc_addr, operator_token, "Deny", req))
