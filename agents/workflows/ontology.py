"""Minimal synchronous accessors over the shared AMH ontology tables
(store/migrations/0001_init.sql) — Goal, Task, Run, Event. Kept deliberately
thin: this is not an ORM, just enough persistence for the durable workflows
in goal.py to log real state instead of only relying on DBOS's own system
tables. See docs/AMH-SPECIFICATION.md Artifact C and E.
"""

from __future__ import annotations

import json
import os
import sqlite3
import uuid
from contextlib import contextmanager
from datetime import datetime, timezone
from typing import Any, Iterator


def now_iso() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


def apply_migrations(db_path: str, migrations_dir: str) -> None:
    """Apply store/migrations/*.sql in filename order, tracked in
    schema_migrations — the same idempotent scheme as daemon/store/store.go,
    reimplemented here so the Python agent layer can bootstrap the ontology
    schema standalone (dev/test) without requiring the Go daemon to have
    run first. In a full deployment the daemon applies migrations before
    the agent layer starts; this is a convenience, not a second authority.
    """
    with connect(db_path) as conn:
        conn.execute(
            """CREATE TABLE IF NOT EXISTS schema_migrations (
                filename TEXT PRIMARY KEY,
                applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
            )"""
        )
        applied = {row[0] for row in conn.execute("SELECT filename FROM schema_migrations")}
        for filename in sorted(os.listdir(migrations_dir)):
            if not filename.endswith(".sql") or filename in applied:
                continue
            with open(os.path.join(migrations_dir, filename)) as f:
                conn.executescript(f.read())
            conn.execute("INSERT INTO schema_migrations (filename) VALUES (?)", (filename,))


@contextmanager
def connect(db_path: str) -> Iterator[sqlite3.Connection]:
    conn = sqlite3.connect(db_path)
    conn.execute("PRAGMA foreign_keys = ON")
    try:
        yield conn
        conn.commit()
    finally:
        conn.close()


def ensure_goal(db_path: str, goal_id: str, text: str, owner: str = "system") -> None:
    with connect(db_path) as conn:
        conn.execute(
            "INSERT OR IGNORE INTO goal (id, text, owner, status) VALUES (?, ?, ?, 'active')",
            (goal_id, text, owner),
        )


def set_goal_status(db_path: str, goal_id: str, status: str) -> None:
    with connect(db_path) as conn:
        conn.execute("UPDATE goal SET status = ? WHERE id = ?", (status, goal_id))


def create_task(db_path: str, goal_id: str, objective: str) -> str:
    task_id = str(uuid.uuid4())
    with connect(db_path) as conn:
        conn.execute(
            "INSERT INTO task (id, goal_id, status) VALUES (?, ?, 'open')",
            (task_id, goal_id),
        )
    return task_id


def set_task_status(db_path: str, task_id: str, status: str) -> None:
    with connect(db_path) as conn:
        conn.execute("UPDATE task SET status = ? WHERE id = ?", (status, task_id))


def create_run(db_path: str, task_id: str | None = None) -> str:
    run_id = str(uuid.uuid4())
    with connect(db_path) as conn:
        conn.execute(
            "INSERT INTO run (id, task_id, started, status) VALUES (?, ?, ?, 'running')",
            (run_id, task_id, now_iso()),
        )
    return run_id


def end_run(db_path: str, run_id: str, status: str = "ok") -> None:
    with connect(db_path) as conn:
        conn.execute(
            "UPDATE run SET ended = ?, status = ? WHERE id = ?",
            (now_iso(), status, run_id),
        )


def log_event(db_path: str, run_id: str, event_type: str, payload: dict[str, Any]) -> None:
    event_id = str(uuid.uuid4())
    with connect(db_path) as conn:
        conn.execute(
            "INSERT INTO event (id, run_id, type, ts, payload) VALUES (?, ?, ?, ?, ?)",
            (event_id, run_id, event_type, now_iso(), json.dumps(payload)),
        )
