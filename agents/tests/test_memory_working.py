"""Tests for memory/working.py's working-memory projection — a query-time
view over the existing goal/task/run/event tables (workflows/ontology.py),
not a separate store. See that module's doc comment.
"""

from __future__ import annotations

from memory import working
from workflows import ontology


def test_project_working_memory_assembles_goal_task_events_and_artifacts(db_path):
    ontology.ensure_goal(db_path, "goal-1", "keep greenhouse healthy")
    task_id = ontology.create_task(db_path, "goal-1", "check vent")
    run_id = ontology.create_run(db_path, task_id=task_id)
    ontology.log_event(db_path, run_id, "tool_call", {"tool": "actuate"})
    ontology.log_event(db_path, run_id, "tool_result", {"outcome": "success"})
    with ontology.connect(db_path) as conn:
        conn.execute("INSERT INTO artifact (id, task_id, uri, hash) VALUES ('a1', %s, 'file://out.log', 'abc')", (task_id,))

    wm = working.project_working_memory(db_path, run_id)

    assert wm.run_id == run_id
    assert wm.run_status == "running"
    assert wm.task_id == task_id
    assert wm.goal_id == "goal-1"
    assert wm.goal_text == "keep greenhouse healthy"
    assert [e["type"] for e in wm.recent_events] == ["tool_result", "tool_call"]  # most recent first
    assert wm.open_artifacts == [{"id": "a1", "uri": "file://out.log", "hash": "abc"}]


def test_project_working_memory_for_run_without_task(db_path):
    run_id = ontology.create_run(db_path)
    wm = working.project_working_memory(db_path, run_id)
    assert wm.task_id is None
    assert wm.goal_id is None
    assert wm.open_artifacts == []


def test_project_working_memory_returns_none_for_unknown_run(db_path):
    assert working.project_working_memory(db_path, "no-such-run") is None


def test_project_working_memory_respects_event_limit(db_path):
    run_id = ontology.create_run(db_path)
    for i in range(5):
        ontology.log_event(db_path, run_id, f"event-{i}", {})
    wm = working.project_working_memory(db_path, run_id, recent_event_limit=2)
    assert len(wm.recent_events) == 2
