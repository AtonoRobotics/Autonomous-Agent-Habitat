"""Working memory (docs/AMH-SPECIFICATION.md §8: "active task/context
projection") — deliberately NOT a stored table. §8 lists working memory
alongside the other four projections but describes it as a projection, not
a store; the ontology it projects over — Goal, Task, Run, Event — already
exists (store/migrations/0001_init.sql) as the core AMH record types §8
itself names as domain-neutral. project_working_memory assembles the
currently-relevant slice of that ontology for one run: its goal, its task,
and its most recent events — the same "what is this run actually doing
right now" context agents/context/compactor.py's compaction rule (§7 rule
6: preserve "the active plan") already assumes exists somewhere.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from memory.store import connect


@dataclass
class WorkingMemory:
    run_id: str
    run_status: str
    task_id: str | None
    task_status: str | None
    goal_id: str | None
    goal_text: str | None
    goal_status: str | None
    recent_events: list[dict[str, Any]]
    open_artifacts: list[dict[str, Any]]


def project_working_memory(db_path: str, run_id: str, recent_event_limit: int = 20) -> WorkingMemory | None:
    import json

    with connect(db_path) as conn:
        run_row = conn.execute("SELECT id, task_id, status FROM run WHERE id = ?", (run_id,)).fetchone()
        if run_row is None:
            return None
        task_id, run_status = run_row[1], run_row[2]

        task_status = goal_id = goal_text = goal_status = None
        open_artifacts: list[dict[str, Any]] = []
        if task_id is not None:
            task_row = conn.execute("SELECT goal_id, status FROM task WHERE id = ?", (task_id,)).fetchone()
            if task_row is not None:
                goal_id, task_status = task_row[0], task_row[1]
                goal_row = conn.execute("SELECT text, status FROM goal WHERE id = ?", (goal_id,)).fetchone()
                if goal_row is not None:
                    goal_text, goal_status = goal_row[0], goal_row[1]
            artifact_rows = conn.execute("SELECT id, uri, hash FROM artifact WHERE task_id = ?", (task_id,)).fetchall()
            open_artifacts = [{"id": r[0], "uri": r[1], "hash": r[2]} for r in artifact_rows]

        event_rows = conn.execute(
            "SELECT id, type, ts, payload FROM event WHERE run_id = ? ORDER BY ts DESC LIMIT ?",
            (run_id, recent_event_limit),
        ).fetchall()
        recent_events = [{"id": r[0], "type": r[1], "ts": r[2], "payload": json.loads(r[3]) if r[3] else None} for r in event_rows]

    return WorkingMemory(
        run_id=run_id,
        run_status=run_status,
        task_id=task_id,
        task_status=task_status,
        goal_id=goal_id,
        goal_text=goal_text,
        goal_status=goal_status,
        recent_events=recent_events,
        open_artifacts=open_artifacts,
    )
