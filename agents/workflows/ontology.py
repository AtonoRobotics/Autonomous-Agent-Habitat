"""Minimal synchronous accessors over the shared AMH ontology tables
(store/migrations/0001_init.sql) — Goal, Task, Run, Event. Kept deliberately
thin: this is not an ORM, just enough persistence for the durable workflows
in goal.py to log real state instead of only relying on DBOS's own system
tables. See docs/AMH-SPECIFICATION.md §1 (decision 4: "Postgres is
authoritative persistent state") and §3.3.

PostgreSQL, via psycopg (the same driver package the `dbos` package itself
depends on) — not SQLite. Every function here takes a Postgres connection
URL (dsn) as its first argument, mirroring daemon/store's own Open(dbURL,
migrationsDir) signature on the Go side; both connect to the same cluster.
"""

from __future__ import annotations

import json
import os
import uuid
from contextlib import contextmanager
from datetime import datetime, timezone
from typing import Any, Iterator

import psycopg


def now_iso() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


def apply_migrations(dsn: str, migrations_dir: str) -> None:
    """Apply store/migrations/*.sql in filename order, tracked in
    schema_migrations — the same idempotent scheme as daemon/store/store.go,
    reimplemented here so the Python agent layer can bootstrap the ontology
    schema standalone (dev/test) without requiring the Go daemon to have
    run first. In a full deployment the daemon applies migrations before
    the agent layer starts; this is a convenience, not a second authority.
    """
    with connect(dsn) as conn:
        conn.execute(
            """CREATE TABLE IF NOT EXISTS schema_migrations (
                filename TEXT PRIMARY KEY,
                applied_at TEXT NOT NULL DEFAULT (to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
            )"""
        )
        applied = {row[0] for row in conn.execute("SELECT filename FROM schema_migrations")}
        for filename in sorted(os.listdir(migrations_dir)):
            if not filename.endswith(".sql") or filename in applied:
                continue
            with open(os.path.join(migrations_dir, filename)) as f:
                conn.execute(f.read())
            conn.execute("INSERT INTO schema_migrations (filename) VALUES (%s)", (filename,))


@contextmanager
def connect(dsn: str) -> Iterator[psycopg.Connection]:
    conn = psycopg.connect(dsn)
    try:
        yield conn
        conn.commit()
    finally:
        conn.close()


def ensure_goal(dsn: str, goal_id: str, text: str, owner: str = "system") -> None:
    with connect(dsn) as conn:
        conn.execute(
            "INSERT INTO goal (id, text, owner, status) VALUES (%s, %s, %s, 'active') ON CONFLICT DO NOTHING",
            (goal_id, text, owner),
        )


def set_goal_status(dsn: str, goal_id: str, status: str) -> None:
    with connect(dsn) as conn:
        conn.execute("UPDATE goal SET status = %s WHERE id = %s", (status, goal_id))


def list_open_goals(dsn: str) -> list[tuple[str, str]]:
    """Every goal still 'open' — read-only. workflows/dispatcher.py relies
    on DBOS's own workflow-id deduplication for exactly-once dispatch
    rather than a mutating claim step here: a mutating claim (flip status
    before starting the workflow) would leave a crash window between
    "claimed" and "workflow actually started" where a goal could get
    stuck claimed-but-never-dispatched. A pure read has no such window —
    see that module's doc comment."""
    with connect(dsn) as conn:
        rows = conn.execute("SELECT id, text FROM goal WHERE status = 'open'").fetchall()
    return [(row[0], row[1]) for row in rows]


def create_task(dsn: str, goal_id: str, objective: str) -> str:
    task_id = str(uuid.uuid4())
    with connect(dsn) as conn:
        conn.execute(
            "INSERT INTO task (id, goal_id, status) VALUES (%s, %s, 'open')",
            (task_id, goal_id),
        )
    return task_id


def set_task_status(dsn: str, task_id: str, status: str) -> None:
    with connect(dsn) as conn:
        conn.execute("UPDATE task SET status = %s WHERE id = %s", (status, task_id))


def create_run(dsn: str, task_id: str | None = None) -> str:
    run_id = str(uuid.uuid4())
    with connect(dsn) as conn:
        conn.execute(
            "INSERT INTO run (id, task_id, started, status) VALUES (%s, %s, %s, 'running')",
            (run_id, task_id, now_iso()),
        )
    return run_id


def end_run(dsn: str, run_id: str, status: str = "ok") -> None:
    with connect(dsn) as conn:
        conn.execute(
            "UPDATE run SET ended = %s, status = %s WHERE id = %s",
            (now_iso(), status, run_id),
        )


def log_event(dsn: str, run_id: str, event_type: str, payload: dict[str, Any]) -> None:
    event_id = str(uuid.uuid4())
    with connect(dsn) as conn:
        conn.execute(
            "INSERT INTO event (id, run_id, type, ts, payload) VALUES (%s, %s, %s, %s, %s)",
            (event_id, run_id, event_type, now_iso(), json.dumps(payload)),
        )
