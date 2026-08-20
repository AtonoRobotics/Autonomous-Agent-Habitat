"""Tests for the pursue_goal / run_subagent durable workflow (Artifact F).

The key property under test is durability: a goal pursued via pursue_goal
must survive a process restart. DBOS keeps workflow state in its system
database (the same SQLite file used for our ontology tables), so a fresh
DBOS() instance pointed at that file recovers and completes any pending
workflow automatically on DBOS.launch().
"""

from __future__ import annotations

import os
import subprocess
import sys
import textwrap
import uuid

import pytest

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
MIGRATIONS_DIR = os.path.join(REPO_ROOT, "store", "migrations")


@pytest.fixture()
def db_path(tmp_path):
    from workflows import ontology

    path = str(tmp_path / "amh.db")
    ontology.apply_migrations(path, MIGRATIONS_DIR)
    return path


def test_pursue_goal_runs_to_completion(db_path):
    from dbos import DBOS

    from workflows.goal import pursue_goal
    from workflows.runtime import init_dbos

    init_dbos("amh-agents-test", db_path)
    DBOS.launch()
    try:
        goal_id = str(uuid.uuid4())
        result = pursue_goal(goal_id, "monitor greenhouse temperature; open vent on threshold", db_path)
        assert "monitor greenhouse temperature" in result
        assert "open vent on threshold" in result
    finally:
        DBOS.destroy()

    import sqlite3

    conn = sqlite3.connect(db_path)
    (status,) = conn.execute("SELECT status FROM goal WHERE id = ?", (goal_id,)).fetchone()
    assert status == "done"
    task_statuses = [row[0] for row in conn.execute("SELECT status FROM task WHERE goal_id = ?", (goal_id,))]
    assert task_statuses == ["done", "done"]
    conn.close()


def test_pursue_goal_survives_process_restart(db_path, tmp_path):
    """Starts pursue_goal *asynchronously* (DBOS.start_workflow) under a
    fixed workflow ID in a subprocess that crashes (os._exit, skipping all
    cleanup and never calling get_result()) immediately after — simulating
    the daemon dying while the workflow is still in flight. A second,
    independent process then calls only DBOS.launch() against the same DB
    file — never re-invoking pursue_goal itself — and DBOS's automatic
    recovery picks up and completes the still-pending workflow on its own.
    This is what proves restart survival: the second process does not ask
    for the goal to be pursued again, yet it ends up done.
    """
    goal_id = str(uuid.uuid4())
    workflow_id = f"pursue-goal-{goal_id}"
    goal_text = "monitor greenhouse temperature; open vent on threshold"

    start_script = textwrap.dedent(f"""
        import sys
        sys.path.insert(0, {REPO_ROOT + "/agents"!r})
        from dbos import DBOS, SetWorkflowID
        from workflows.goal import pursue_goal
        from workflows.runtime import init_dbos

        init_dbos("amh-agents-test", {db_path!r})
        DBOS.launch()
        with SetWorkflowID({workflow_id!r}):
            DBOS.start_workflow(pursue_goal, {goal_id!r}, {goal_text!r}, {db_path!r})
        # Crash immediately: no get_result(), no DBOS.destroy(). The
        # workflow is durably registered as PENDING but has not necessarily
        # run any steps yet — recovery must be able to start it from
        # scratch just as well as resume it partway through.
        import os
        os._exit(1)
    """)
    start_result = subprocess.run(
        [sys.executable, "-c", start_script],
        cwd=os.path.join(REPO_ROOT, "agents"),
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert start_result.returncode == 1, start_result.stderr

    resume_script = textwrap.dedent(f"""
        import sys
        sys.path.insert(0, {REPO_ROOT + "/agents"!r})
        from dbos import DBOS
        from workflows.goal import pursue_goal  # noqa: F401 (registers the workflow)
        from workflows.runtime import init_dbos

        init_dbos("amh-agents-test", {db_path!r})
        DBOS.launch()  # triggers automatic recovery of the pending workflow
        handle = DBOS.retrieve_workflow({workflow_id!r})
        result = handle.get_result()
        print("RESULT:" + result)
        DBOS.destroy()
    """)
    resume_result = subprocess.run(
        [sys.executable, "-c", resume_script],
        cwd=os.path.join(REPO_ROOT, "agents"),
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert resume_result.returncode == 0, resume_result.stderr
    assert "RESULT:" in resume_result.stdout
    assert "monitor greenhouse temperature" in resume_result.stdout

    import sqlite3

    conn = sqlite3.connect(db_path)
    (status,) = conn.execute("SELECT status FROM goal WHERE id = ?", (goal_id,)).fetchone()
    assert status == "done"
    conn.close()
