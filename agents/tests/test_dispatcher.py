"""Tests for workflows/dispatcher.py — the goal dispatcher that connects
a durably-created `goal` row (status 'open', exactly what daemon/a2a's
SendMessage inserts) to actually running pursue_goal for it.
"""

from __future__ import annotations

import uuid

import psycopg


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
