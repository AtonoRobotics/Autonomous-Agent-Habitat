"""DBOS runtime bootstrap for the AMH agent layer. See
docs/AMH-SPECIFICATION.md §1 (decision 3: "DBOS is the sole durable
workflow engine in V1"; decision 4: "Postgres is authoritative persistent
state") — this module wires up DBOS directly, the only durability engine
AMH core runs today. A swappable DurableEngine Protocol is the abstraction
a second engine (Temporal) would introduce; nothing in this codebase
depends on that abstraction existing before then.

DBOS's own system tables and the AMH ontology (store/migrations) share the
same PostgreSQL cluster — see daemon/store's doc comment. dbos_url is
already a complete postgresql:// connection URL; DBOS/SQLAlchemy needs no
translation the way SQLite's file-path-to-sqlite:/// scheme once did.

If dbos_url carries a `-c search_path=<schema>` libpq option (as this
repo's test fixtures use for per-test isolation — see agents/tests/
conftest.py's db_path fixture), DBOS's own system tables are pinned into
that same schema (system_database_url = the same URL, dbos_system_schema =
that schema) instead of DBOS's own default of deriving a separate
`<dbname>_dbos_sys` database — so a test's DROP SCHEMA ... CASCADE cleans
up DBOS's workflow state too, not just AMH's own tables. Production
DATABASE_URL carries no such option, so this is a no-op there: DBOS just
uses its own default "dbos" schema in the one real database.
"""

from __future__ import annotations

from urllib.parse import parse_qs, unquote, urlparse

from dbos import DBOS, DBOSConfig


def _search_path_schema(dbos_url: str) -> str | None:
    query = parse_qs(urlparse(dbos_url).query)
    options = query.get("options", [None])[0]
    if not options:
        return None
    for token in unquote(options).split():
        if token.startswith("search_path="):
            return token[len("search_path=") :]
    return None


def init_dbos(app_name: str, dbos_url: str, run_admin_server: bool = False) -> DBOS:
    config: DBOSConfig = {
        "name": app_name,
        "database_url": dbos_url,
        "run_admin_server": run_admin_server,
    }
    schema = _search_path_schema(dbos_url)
    if schema is not None:
        config["system_database_url"] = dbos_url
        config["dbos_system_schema"] = schema
    return DBOS(config=config)
