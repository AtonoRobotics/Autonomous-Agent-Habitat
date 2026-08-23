"""Self-improvement candidate lifecycle gRPC read client over
daemon/grpcapi's SelfImproveService (daemon/selfimprove —
docs/AMH-SPECIFICATION.md §10).

Phase 3 of the gRPC migration (see workflows/policy.py's module docstring
for Phase 1, workflows/operations.py's for Phase 2, and
contracts/proto/selfimprove.proto's header comment for this phase's
scope): only the two read routes this module actually calls
(GetCandidate isn't used here, but ListCandidates is) moved to gRPC — the
operator-only write routes (Generate/RecordEval/Canary/Promote/Demote/
Rollback/Reject) have no Python caller and stay on daemon/api's HTTP
surface.

get_promoted_prompt is the first real call site anywhere in this
codebase that reads from a promoted CandidateVersion rather than just
managing its bookkeeping — closing §10's "promotion uses the real
extension lifecycle... a real promoted candidate actually changes what
a live call site does." Promote's own DB-level guarantee (a partial
unique index plus a per-class advisory lock — see daemon/selfimprove's
doc comment) means at most one candidate of a given class can ever be
simultaneously 'promoted', so there is never an ambiguous "which one"
to pick among more than one result.

Scope, stated plainly: CandidateVersion has no field beyond the coarse
candidate_class enum ("prompt", "retrieval_policy", "skill", "module",
"core_code") to distinguish WHICH prompt a "prompt"-class candidate
targets. That's fine with exactly one real prompt call site in this
codebase (workflows/goal.py's decompose_goal) — "the promoted prompt
candidate" and "decompose_goal's prompt" are the same thing today — but
this does not yet generalize to a habitat with more than one
independently-promotable prompt. A real, later gap, not a silent
decision.
"""

from __future__ import annotations

import threading

import grpc

from selfimprove.selfimprovepb import selfimprove_pb2, selfimprove_pb2_grpc


class SelfImproveError(Exception):
    pass


# See workflows/policy.py's identical _channels/_channel pattern.
_channels: dict[str, grpc.Channel] = {}
_channels_lock = threading.Lock()


def _channel(daemon_grpc_addr: str) -> grpc.Channel:
    with _channels_lock:
        channel = _channels.get(daemon_grpc_addr)
        if channel is None:
            channel = grpc.insecure_channel(daemon_grpc_addr)
            _channels[daemon_grpc_addr] = channel
        return channel


def _candidate_to_dict(c: selfimprove_pb2.CandidateVersion) -> dict:
    return {
        "id": c.id,
        "candidate_class": c.candidate_class,
        "ref": c.ref,
        "status": c.status,
        "generated_by": c.generated_by,
        "created_at": c.created_at,
        "canary_at": c.canary_at,
        "promoted_at": c.promoted_at,
        "demoted_at": c.demoted_at,
        "rolled_back_at": c.rolled_back_at,
        "rollback_target_id": c.rollback_target_id,
    }


def list_candidates(daemon_grpc_addr: str, agent_token: str, candidate_class: str = "", status: str = "") -> list[dict]:
    """Real, agent-accessible read via ListCandidates, optionally filtered
    by candidate_class/status."""
    stub = selfimprove_pb2_grpc.SelfImproveServiceStub(_channel(daemon_grpc_addr))
    req = selfimprove_pb2.ListCandidatesRequest(candidate_class=candidate_class, status=status)
    metadata = (("authorization", f"Bearer {agent_token}"),)
    try:
        resp = stub.ListCandidates(req, metadata=metadata, timeout=10)
    except grpc.RpcError as e:
        raise SelfImproveError(e.details() or e.code().name) from e
    return [_candidate_to_dict(c) for c in resp.candidates]


def get_promoted_prompt(daemon_grpc_addr: str, agent_token: str, default: str) -> str:
    """Returns the currently promoted "prompt"-class candidate's ref
    (its real content) if one exists, otherwise default — never raises
    for the ordinary "nothing promoted yet" case, since a habitat with
    no self-improvement optimizer running at all (the common case today
    — see daemon/selfimprove's own doc comment on this) must fall back
    to its hardcoded prompt exactly as it always has, not fail the call
    that needs a prompt to proceed."""
    candidates = list_candidates(daemon_grpc_addr, agent_token, candidate_class="prompt", status="promoted")
    if not candidates:
        return default
    return candidates[0]["ref"]
