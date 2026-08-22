from __future__ import annotations

import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from threading import Thread

import pytest

from context.budget import BudgetManager, Turn
from context.compactor import Checkpoint, Compactor, extractive_summarize, llm_summarize
from context.llm import ModelClient


def test_budget_tracks_usage_fraction():
    budget = BudgetManager(window_budget=100)
    budget.add_turn("user", "a" * 40)  # ~10 tokens at chars//4
    assert budget.used_tokens == 10
    assert budget.fraction_used == 0.10
    assert not budget.over_compact_threshold


def test_budget_crosses_compact_threshold():
    budget = BudgetManager(window_budget=100, compact_at=0.70)
    budget.add_turn("user", "a" * 400)  # 100 tokens > 70% of 100
    assert budget.over_compact_threshold


def test_tool_result_cap_truncates_and_flags():
    budget = BudgetManager(tool_result_cap=10)  # 10 tokens = 40 chars
    content = "x" * 1000
    turn = budget.add_turn("tool", content, is_tool_result=True)
    assert turn.truncated is True
    assert turn.tokens <= 10
    assert len(turn.content) == 40


def test_tool_result_under_cap_is_not_truncated():
    budget = BudgetManager(tool_result_cap=1000)
    turn = budget.add_turn("tool", "short result", is_tool_result=True)
    assert turn.truncated is False
    assert turn.content == "short result"


def test_compactor_noop_below_threshold():
    budget = BudgetManager(window_budget=1_000_000, compact_at=0.70)
    budget.add_turn("user", "hello")
    result = Compactor().compact(budget)
    assert result is None
    assert len(budget.turns) == 1


def test_compactor_summarizes_oldest_keeps_recent_verbatim():
    budget = BudgetManager(window_budget=100, compact_at=0.70)
    for i in range(10):
        budget.add_turn("user" if i % 2 == 0 else "assistant", f"turn {i} " * 5)

    assert budget.over_compact_threshold
    original_turns = list(budget.turns)

    compactor = Compactor(keep_recent_turns_raw=3)
    result = compactor.compact(budget)

    assert result is not None
    assert result.turns_compacted == 7
    # New shape: [summary, *3 verbatim recent turns]
    assert len(budget.turns) == 4
    assert budget.turns[0].role == "system"
    assert budget.turns[1:] == original_turns[-3:]


def test_extractive_summarize_recovers_goal_from_the_passed_in_objective():
    """§7 rule 6 / acceptance invariant #8: compaction must preserve
    "goal" — but budget.turns never contains it (the harness passes the
    objective separately as the system prompt, never as a turn), so
    extractive_summarize can only recover it if the caller passes it in
    explicitly."""
    turns = [Turn(role="user", content="hello", tokens=1)]
    checkpoint = extractive_summarize(turns, objective="water the greenhouse plants")
    assert checkpoint.goal == "water the greenhouse plants"


def test_extractive_summarize_recovers_failures_from_error_prefixed_turns():
    """agentic_loop.py's own established convention: a failed tool call's
    result is a turn whose content is prefixed "error: " (see
    _run_agentic_loop_async's mcp/builtin-tool except branches) — a real,
    mechanical signal extractive_summarize can use without any semantic
    understanding, unlike the fields it genuinely can't recover offline."""
    turns = [
        Turn(role="user", content="error: file not found: /tmp/x.txt", tokens=1),
        Turn(role="assistant", content="ok, trying again", tokens=1),
        Turn(role="user", content="error: connection refused", tokens=1),
    ]
    checkpoint = extractive_summarize(turns, objective="do the thing")
    assert checkpoint.failures == ["file not found: /tmp/x.txt", "connection refused"]


def test_extractive_summarize_recovers_artifact_references_via_path_and_uri_scan():
    """Another genuinely mechanical, syntactic-only extraction: turn
    content mentioning a file path or a URI (e.g. VFS's own "wrote
    {path}" tool-result convention, or an artifact:// reference) is
    real, recoverable data — not a semantic judgment call."""
    turns = [
        Turn(role="user", content="wrote /workspace/notes/plan.md", tokens=1),
        Turn(role="user", content="fetched artifact://run-42/report.json for review", tokens=1),
        Turn(role="assistant", content="looks good, no paths mentioned here", tokens=1),
    ]
    checkpoint = extractive_summarize(turns, objective="write a plan")
    assert "/workspace/notes/plan.md" in checkpoint.artifact_references
    assert "artifact://run-42/report.json" in checkpoint.artifact_references


def test_extractive_summarize_leaves_semantically_hard_fields_none_not_fabricated():
    """The offline strategy has no semantic understanding, so fields it
    can't genuinely recover must stay honestly absent, not guessed at —
    the same fail-honest posture ModelNotConfiguredError already takes
    elsewhere in this codebase, rather than silently inventing content."""
    turns = [Turn(role="user", content="just some ordinary conversation", tokens=1)]
    checkpoint = extractive_summarize(turns, objective="do a thing")
    assert checkpoint.governing_constraints is None
    assert checkpoint.completion_predicate is None
    assert checkpoint.unresolved_decisions == []
    assert checkpoint.uncertainty is None
    assert checkpoint.active_plan is None


def test_compact_threads_the_objective_into_the_checkpoint_and_serializes_it_for_storage():
    """compact() must accept the caller's objective (budget.turns never
    contains it) and pass it through to the summarize strategy so
    result.summary is a real Checkpoint, per §7 rule 6 / acceptance
    invariant #8 — and the stored system turn's content must be that
    checkpoint's JSON serialization, not an opaque string, so a later
    reader (or a rehydrated agent) can recover the structured fields."""
    budget = BudgetManager(window_budget=100, compact_at=0.70)
    for i in range(10):
        budget.add_turn("user", f"turn {i} " * 5)

    compactor = Compactor(keep_recent_turns_raw=3)
    result = compactor.compact(budget, objective="water the greenhouse plants")

    assert result is not None
    assert isinstance(result.summary, Checkpoint)
    assert result.summary.goal == "water the greenhouse plants"

    stored = json.loads(budget.turns[0].content)
    assert stored["goal"] == "water the greenhouse plants"


def test_compaction_is_idempotent_shape_across_repeated_runs():
    """After compaction, the turn list is [summary, verbatim...] — running
    compact() again (once enough new turns accumulate) must produce the
    same stable shape, not a growing chain of nested summaries, per the
    cache-stable-prefix discipline in §3."""
    budget = BudgetManager(window_budget=100, compact_at=0.70)
    for i in range(10):
        budget.add_turn("user", f"turn {i} " * 5)
    compactor = Compactor(keep_recent_turns_raw=3)
    compactor.compact(budget)
    assert len(budget.turns) == 4

    for i in range(10, 20):
        budget.add_turn("user", f"turn {i} " * 5)
    compactor.compact(budget)
    assert budget.turns[0].role == "system"
    assert len(budget.turns) == 4


class _FakeDaemon(BaseHTTPRequestHandler):
    """Stands in for the daemon's inference seam, same pattern as
    test_llm.py's fixture — a real HTTP server, not a mocked ModelClient,
    so llm_summarize is verified against the real request/response cycle
    it will actually run in production."""

    response_body = b"{}"

    def log_message(self, format, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(type(self).response_body)


@pytest.fixture()
def fake_daemon():
    server = HTTPServer(("127.0.0.1", 0), _FakeDaemon)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    _FakeDaemon.response_body = b"{}"
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        thread.join(timeout=5)


def test_llm_summarize_parses_the_models_structured_checkpoint_json(fake_daemon):
    """llm_summarize is the model-driven strategy — unlike
    extractive_summarize, it can genuinely populate every Checkpoint field,
    since a real model has the semantic understanding the offline strategy
    lacks. It must request and parse all 8 fields as structured JSON, not
    return a bare string."""
    model_response = {
        "goal": "water the greenhouse plants",
        "governing_constraints": "never exceed 2L per plant",
        "completion_predicate": "all plants show moist soil",
        "unresolved_decisions": ["which watering schedule to use"],
        "failures": ["pump stalled once"],
        "uncertainty": "sensor calibration may be off",
        "active_plan": "water zone A then zone B",
        "artifact_references": ["/workspace/notes/plan.md"],
    }
    _FakeDaemon.response_body = json.dumps({"text": json.dumps(model_response)}).encode()
    client = ModelClient(daemon_api_base_url=fake_daemon, agent_token="tok", model="claude-sonnet-5", provider="anthropic")
    turns = [Turn(role="user", content="watered zone A", tokens=1)]

    checkpoint = llm_summarize(client)(turns, "water the greenhouse plants")

    assert checkpoint == Checkpoint(**model_response)


def test_llm_summarize_raises_on_non_json_response(fake_daemon):
    """Same fail-honest posture as decompose_goal: a malformed model
    response is a real error, never silently swapped for an empty or
    fabricated Checkpoint."""
    _FakeDaemon.response_body = json.dumps({"text": "not json at all"}).encode()
    client = ModelClient(daemon_api_base_url=fake_daemon, agent_token="tok", model="claude-sonnet-5", provider="anthropic")
    turns = [Turn(role="user", content="hello", tokens=1)]

    with pytest.raises(ValueError, match="not valid JSON"):
        llm_summarize(client)(turns, "do a thing")
