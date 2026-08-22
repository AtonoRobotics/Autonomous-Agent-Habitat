"""Tests for workflows/dispatcher.py — the goal dispatcher that connects
a durably-created `goal` row (status 'open', exactly what daemon/a2a's
SendMessage inserts) to actually running pursue_goal for it, and cancels
an in-flight one for real once daemon/a2a's CancelTask marks it
'canceled'.
"""

from __future__ import annotations

import json
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import psycopg
import pytest


def _insert_open_goal(db_path: str, text: str) -> str:
    """Inserts a goal exactly the way daemon/a2a's CreateTaskFromMessage
    does (status 'open') — raw SQL rather than workflows.ontology, since
    ontology.ensure_goal hardcodes status 'active' for a different
    call path (goal.py's own decompose_goal)."""
    goal_id = str(uuid.uuid4())
    conn = psycopg.connect(db_path)
    conn.execute("INSERT INTO goal (id, text, status) VALUES (%s, %s, 'open')", (goal_id, text))
    conn.commit()
    conn.close()
    return goal_id


def test_claim_and_dispatch_once_runs_an_open_goal_to_completion(db_path, daemon, fake_model_server):
    from dbos import DBOS

    from workflows.dispatcher import claim_and_dispatch_once
    from workflows.goal import pursue_goal  # noqa: F401 (registers the workflow)
    from workflows.runtime import init_dbos

    init_dbos("amh-dispatcher-test", db_path)
    DBOS.launch()
    try:
        goal_id = _insert_open_goal(db_path, "monitor greenhouse temperature; open vent on threshold")

        dispatched = claim_and_dispatch_once(db_path, daemon.base_url, daemon.agent_token)
        assert dispatched == [goal_id]

        handle = DBOS.retrieve_workflow(f"goal-{goal_id}")
        result = handle.get_result()
        assert "monitor greenhouse temperature" in result
    finally:
        DBOS.destroy()

    conn = psycopg.connect(db_path)
    (status,) = conn.execute("SELECT status FROM goal WHERE id = %s", (goal_id,)).fetchone()
    assert status == "done"
    conn.close()


def test_claim_and_dispatch_once_ignores_non_open_goals(db_path, daemon, fake_model_server):
    from dbos import DBOS

    from workflows.dispatcher import claim_and_dispatch_once
    from workflows.goal import pursue_goal  # noqa: F401
    from workflows.runtime import init_dbos

    init_dbos("amh-dispatcher-test", db_path)
    DBOS.launch()
    try:
        conn = psycopg.connect(db_path)
        active_id = str(uuid.uuid4())
        conn.execute("INSERT INTO goal (id, text, status) VALUES (%s, %s, 'active')", (active_id, "already running"))
        done_id = str(uuid.uuid4())
        conn.execute("INSERT INTO goal (id, text, status) VALUES (%s, %s, 'done')", (done_id, "already finished"))
        conn.commit()
        conn.close()

        dispatched = claim_and_dispatch_once(db_path, daemon.base_url, daemon.agent_token)
        assert dispatched == []
    finally:
        DBOS.destroy()


def test_claim_and_dispatch_once_is_idempotent_under_a_repeat_call(db_path, daemon, fake_model_server):
    """Calling it twice for the same still-open goal (simulating two
    dispatcher polls racing, or the same poll seeing a goal that hasn't
    finished yet) must not double-run pursue_goal — DBOS's own
    workflow-id dedup, not a mutating claim step, is what this module
    relies on (see its doc comment)."""
    from dbos import DBOS

    from workflows.dispatcher import claim_and_dispatch_once
    from workflows.goal import pursue_goal  # noqa: F401
    from workflows.runtime import init_dbos

    init_dbos("amh-dispatcher-test", db_path)
    DBOS.launch()
    try:
        goal_id = _insert_open_goal(db_path, "keep the greenhouse healthy overnight")

        first = claim_and_dispatch_once(db_path, daemon.base_url, daemon.agent_token)
        # Nothing has marked the goal non-'open' yet (by design — see the
        # module doc comment), so the second call sees it again.
        second = claim_and_dispatch_once(db_path, daemon.base_url, daemon.agent_token)
        assert first == [goal_id]
        assert second == [goal_id]

        handle = DBOS.retrieve_workflow(f"goal-{goal_id}")
        result = handle.get_result()
        assert "keep the greenhouse healthy overnight" in result
    finally:
        DBOS.destroy()

    conn = psycopg.connect(db_path)
    (task_count,) = conn.execute("SELECT COUNT(*) FROM task WHERE goal_id = %s", (goal_id,)).fetchone()
    conn.close()
    # decompose_goal ran exactly once (it's a memoized DBOS step under one
    # workflow id) — a real double-dispatch would show up as double the
    # tasks (decompose_goal called twice, each creating its own set). This
    # goal text has no ";" clause separator, so it decomposes to one task.
    assert task_count == 1


_SUBAGENT_RESPONSE_DELAY_SEC = 3.0


class _SlowSubagentModelHandler(BaseHTTPRequestHandler):
    """Mirrors conftest.py's _FakeModelHandler exactly, except the
    agentic-loop ("done") branch — do_subagent_work's real model call —
    sleeps first. decompose_goal's own call is left fast, so pursue_goal
    reaches start_subagent quickly and blocks in h.get_result() waiting
    on a child run_subagent workflow that is genuinely still in flight,
    giving a cancellation test a real window to act within, not a race
    against an already-finished goal."""

    def log_message(self, format, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length))
        messages = body["messages"]
        system_content = messages[0]["content"] if messages and messages[0]["role"] == "system" else ""
        user_content = next(m["content"] for m in reversed(messages) if m["role"] == "user")

        agentic_marker = "Your task:\n\n"
        if "JSON array" in system_content:
            clauses = [c.strip() for c in user_content.split(";") if c.strip()] or [user_content]
            content = json.dumps([{"objective": c} for c in clauses])
        elif agentic_marker in system_content:
            time.sleep(_SUBAGENT_RESPONSE_DELAY_SEC)
            objective = system_content.split(agentic_marker, 1)[1]
            content = json.dumps({"tool": "done", "result": f"completed: {objective}"})
        else:
            content = f"completed: {user_content}"

        response = json.dumps({"choices": [{"message": {"role": "assistant", "content": content}}]}).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(response)))
        self.end_headers()
        self.wfile.write(response)


@pytest.fixture()
def slow_subagent_model_server(daemon, monkeypatch):
    """Same registration dance as conftest.py's fake_model_server fixture
    (see register_model_provider_account's own doc comment for why it's
    shared), but backed by _SlowSubagentModelHandler instead."""
    from conftest import _find_free_port, register_model_provider_account

    port = _find_free_port()
    server = ThreadingHTTPServer(("127.0.0.1", port), _SlowSubagentModelHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    register_model_provider_account(daemon, f"http://127.0.0.1:{port}")
    monkeypatch.setenv("ADAPTER_MODEL", "test-fake-model")
    monkeypatch.setenv("ADAPTER_PROVIDER", "test-fake")

    try:
        yield f"http://127.0.0.1:{port}"
    finally:
        server.shutdown()
        thread.join(timeout=5)


def test_cancel_interrupted_goals_once_actually_stops_an_in_flight_run(db_path, daemon, slow_subagent_model_server):
    """The real point of this module's other half: A2A's CancelTask
    already marks the goal row 'canceled' synchronously (simulated here
    with raw SQL, exactly like daemon/a2a.Store.CancelTask does) — this
    test proves cancel_interrupted_goals_once turns that into a genuine
    DBOS-level stop, not just a cosmetic status flip, by cancelling a
    goal whose subagent is still genuinely blocked in a slow model call
    and confirming pursue_goal's own handle raises well before that slow
    call would have finished on its own."""
    from dbos import DBOS
    from dbos._error import DBOSAwaitedWorkflowCancelledError

    from workflows.dispatcher import cancel_interrupted_goals_once, claim_and_dispatch_once
    from workflows.goal import pursue_goal  # noqa: F401
    from workflows.runtime import init_dbos

    init_dbos("amh-dispatcher-cancel-test", db_path)
    DBOS.launch()
    try:
        goal_id = _insert_open_goal(db_path, "water the greenhouse plants")
        dispatched = claim_and_dispatch_once(db_path, daemon.base_url, daemon.agent_token)
        assert dispatched == [goal_id]

        # Give pursue_goal time to run decompose_goal (fast) and reach
        # start_subagent, so the child run_subagent workflow is genuinely
        # in flight (blocked in its own slow model call) before we cancel.
        time.sleep(0.5)

        conn = psycopg.connect(db_path)
        conn.execute("UPDATE goal SET status = 'canceled' WHERE id = %s", (goal_id,))
        conn.commit()
        conn.close()

        canceled = cancel_interrupted_goals_once(db_path)
        assert canceled == [goal_id]

        handle = DBOS.retrieve_workflow(f"goal-{goal_id}")
        started_at = time.monotonic()
        with pytest.raises(DBOSAwaitedWorkflowCancelledError):
            handle.get_result()
        elapsed = time.monotonic() - started_at
        # A genuine stop returns almost immediately; an uninterrupted run
        # would only resolve after _SUBAGENT_RESPONSE_DELAY_SEC.
        assert elapsed < _SUBAGENT_RESPONSE_DELAY_SEC
    finally:
        DBOS.destroy()


def test_cancel_interrupted_goals_once_ignores_non_canceled_goals(db_path, daemon, fake_model_server):
    from dbos import DBOS

    from workflows.dispatcher import cancel_interrupted_goals_once
    from workflows.goal import pursue_goal  # noqa: F401
    from workflows.runtime import init_dbos

    init_dbos("amh-dispatcher-cancel-test", db_path)
    DBOS.launch()
    try:
        conn = psycopg.connect(db_path)
        open_id = str(uuid.uuid4())
        conn.execute("INSERT INTO goal (id, text, status) VALUES (%s, %s, 'open')", (open_id, "still open"))
        done_id = str(uuid.uuid4())
        conn.execute("INSERT INTO goal (id, text, status) VALUES (%s, %s, 'done')", (done_id, "already finished"))
        conn.commit()
        conn.close()

        assert cancel_interrupted_goals_once(db_path) == []
    finally:
        DBOS.destroy()


def test_cancel_interrupted_goals_once_is_idempotent_under_a_repeat_call(db_path, daemon):
    """Calling DBOS.cancel_workflow twice for the same goal (two
    dispatcher polls, or a goal that stays 'canceled' across several
    ticks) must not raise or otherwise misbehave — see this module's doc
    comment for why the underlying call is a plain, safe SQL UPDATE."""
    from dbos import DBOS

    from workflows.dispatcher import cancel_interrupted_goals_once
    from workflows.goal import pursue_goal  # noqa: F401
    from workflows.runtime import init_dbos

    init_dbos("amh-dispatcher-cancel-test", db_path)
    DBOS.launch()
    try:
        conn = psycopg.connect(db_path)
        goal_id = str(uuid.uuid4())
        conn.execute("INSERT INTO goal (id, text, status) VALUES (%s, %s, 'canceled')", (goal_id, "never started"))
        conn.commit()
        conn.close()

        first = cancel_interrupted_goals_once(db_path)
        second = cancel_interrupted_goals_once(db_path)
        assert first == [goal_id]
        assert second == [goal_id]
    finally:
        DBOS.destroy()
