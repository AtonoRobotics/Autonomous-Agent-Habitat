"""DBOS runtime bootstrap for the AMH agent layer. See
docs/AMH-SPECIFICATION.md §2 (DBOS Transact, SQLite primary) and the
DurableEngine port in Artifact B — this module wires up DBOS directly, the
only durability engine AMH core runs today (docs/AMH-SPECIFICATION.md §1
decision 3). A swappable DurableEngine Protocol is the abstraction a
second engine (Temporal) would introduce; nothing in this codebase depends
on that abstraction existing before then.
"""

from __future__ import annotations

import os

from dbos import DBOS, DBOSConfig


def sqlite_url(db_path: str) -> str:
    """Convert a filesystem path into the sqlite:/// URL DBOS/SQLAlchemy
    expects. Mirrors DATABASE_URL=sqlite:./state/amh.db from .env.example,
    but DBOS wants an explicit sqlite:/// scheme with an absolute path."""
    abs_path = os.path.abspath(db_path)
    return f"sqlite:///{abs_path}"


def init_dbos(app_name: str, db_path: str, run_admin_server: bool = False) -> DBOS:
    config: DBOSConfig = {
        "name": app_name,
        "database_url": sqlite_url(db_path),
        "run_admin_server": run_admin_server,
    }
    return DBOS(config=config)
