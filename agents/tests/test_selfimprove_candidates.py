"""Tests for selfimprove/candidates.py (daemon/grpcapi's
SelfImproveService.ListCandidates over real gRPC — the write-side
candidate lifecycle driven directly here still uses daemon/api's HTTP
routes, since nothing besides this test module calls them) and for
decompose_goal's real use of get_promoted_prompt — the first live
capability switch on a promoted CandidateVersion anywhere in this
codebase (§10).
"""

from __future__ import annotations

import json
import threading
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from conftest import _find_free_port, register_model_provider_account
from selfimprove.candidates import get_promoted_prompt, list_candidates


def _post(daemon, path: str, body: dict, token: str) -> dict:
    request = urllib.request.Request(
        f"{daemon.base_url}{path}",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=10) as resp:
        return json.loads(resp.read())


def _promote_prompt_candidate(daemon, ref: str) -> str:
    """Drives a real "prompt"-class candidate through the real
    daemon/selfimprove lifecycle (Generate -> RecordEval -> Canary ->
    RecordEval -> Promote) exactly as an operator would, and returns its
    id. Mirrors daemon/selfimprove/selfimprove_test.go's own
    promoteThroughCanary helper — same real sequence, over HTTP instead
    of the Go-internal seam."""
    candidate = _post(daemon, "/v1/selfimprove/candidates", {"candidate_class": "prompt", "ref": ref}, daemon.agent_token)
    candidate_id = candidate["id"]

    eval_body = {"evaluator_id": "eval-suite", "evaluator_version": "1.0.0", "case_results": [True] * 5}
    _post(daemon, f"/v1/selfimprove/candidates/{candidate_id}/eval", eval_body, daemon.operator_token)
    _post(daemon, f"/v1/selfimprove/candidates/{candidate_id}/canary", {}, daemon.operator_token)
    _post(daemon, f"/v1/selfimprove/candidates/{candidate_id}/eval", eval_body, daemon.operator_token)
    promoted = _post(daemon, f"/v1/selfimprove/candidates/{candidate_id}/promote", {}, daemon.operator_token)
    assert promoted["status"] == "promoted"
    return candidate_id


def test_get_promoted_prompt_returns_default_when_nothing_promoted(daemon):
    result = get_promoted_prompt(daemon.grpc_addr, daemon.agent_token, "the hardcoded default")
    assert result == "the hardcoded default"


def test_get_promoted_prompt_returns_the_real_promoted_content(daemon):
    _promote_prompt_candidate(daemon, "a genuinely different, real promoted prompt")

    result = get_promoted_prompt(daemon.grpc_addr, daemon.agent_token, "the hardcoded default")

    assert result == "a genuinely different, real promoted prompt"


def test_list_candidates_filters_by_class_and_status(daemon):
    _promote_prompt_candidate(daemon, "filtered prompt")

    promoted = list_candidates(daemon.grpc_addr, daemon.agent_token, candidate_class="prompt", status="promoted")
    assert len(promoted) == 1
    assert promoted[0]["ref"] == "filtered prompt"

    none_generated = list_candidates(daemon.grpc_addr, daemon.agent_token, candidate_class="prompt", status="generated")
    assert none_generated == []


class _CapturingModelHandler(BaseHTTPRequestHandler):
    """Stands in for a model provider, capturing whichever system prompt
    it actually received rather than asserting on canned response
    content keyed to specific prompt text (conftest.py's shared
    fake_model_server does that, so it can't distinguish "the hardcoded
    default" from "a real promoted prompt" — this test's whole point is
    telling those apart). Always answers with a valid decompose-shaped
    JSON array, regardless of which prompt was sent."""

    captured_system = None

    def log_message(self, format, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length))
        messages = body.get("messages", [])
        type(self).captured_system = messages[0]["content"] if messages and messages[0]["role"] == "system" else ""
        response = json.dumps({"choices": [{"message": {"role": "assistant", "content": json.dumps([{"objective": "task"}])}}]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(response)))
        self.end_headers()
        self.wfile.write(response)


@pytest.fixture()
def capturing_model_server(daemon, monkeypatch):
    _CapturingModelHandler.captured_system = None
    port = _find_free_port()
    server = ThreadingHTTPServer(("127.0.0.1", port), _CapturingModelHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    register_model_provider_account(daemon, f"http://127.0.0.1:{port}", provider="test-fake-capture")
    monkeypatch.setenv("ADAPTER_MODEL", "test-fake-model")
    monkeypatch.setenv("ADAPTER_PROVIDER", "test-fake-capture")
    try:
        yield
    finally:
        server.shutdown()
        thread.join(timeout=5)


def test_decompose_goal_uses_the_real_promoted_prompt_not_the_hardcoded_default(daemon, db_path, capturing_model_server):
    """The actual live-capability-switch proof: a real promoted
    CandidateVersion changes what decompose_goal's real gRPC call to the
    daemon's inference seam actually sends — not just durable
    bookkeeping nobody reads."""
    from workflows.goal import decompose_goal

    _promote_prompt_candidate(daemon, "PROMOTED SYSTEM PROMPT — respond with a JSON array of one task.")

    decompose_goal("goal-1", "do the thing", db_path, daemon.grpc_addr, daemon.agent_token)

    assert _CapturingModelHandler.captured_system == "PROMOTED SYSTEM PROMPT — respond with a JSON array of one task."


def test_decompose_goal_falls_back_to_the_hardcoded_default_when_nothing_promoted(daemon, db_path, capturing_model_server):
    from workflows.goal import _DECOMPOSE_SYSTEM_PROMPT, decompose_goal

    decompose_goal("goal-2", "do another thing", db_path, daemon.grpc_addr, daemon.agent_token)

    assert _CapturingModelHandler.captured_system == _DECOMPOSE_SYSTEM_PROMPT
