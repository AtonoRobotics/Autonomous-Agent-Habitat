"""Shared fixtures for end-to-end tests that need a real Go daemon. See
daemon/cmd/amh-daemon.

Also provides fake_model_server: a real local HTTP server standing in for
a model provider, registered as a real account against a real running
daemon (daemon/inference, daemon/credentials) exactly the way an operator
would register a real provider — so workflows.goal's real model calls
travel the actual path (agent token -> daemon -> provider account
credential -> HTTP call) end to end, not a shortcut through the agent
process's own environment. The decomposition/completion logic this server
returns is deliberately test-fixture logic, standing in for what a real
model would answer for these specific deterministic test inputs — it does
not reintroduce placeholder logic into workflows/goal.py or
daemon/inference itself.
"""

from __future__ import annotations

import json
import os
import secrets
import subprocess
import threading
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import quote, urlparse, urlunparse

import psycopg
import pytest

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
MIGRATIONS_DIR = os.path.join(REPO_ROOT, "store", "migrations")

# PostgreSQL is authoritative persistent state (§1 decision 4) — not SQLite.
# Every test gets its own schema in this shared instance (the same
# schema-per-test isolation daemon/store/storetest uses on the Go side),
# created fresh and dropped on cleanup. Override for a non-default local
# Postgres via AMH_TEST_DATABASE_URL.
TEST_POSTGRES_ADMIN_URL = os.environ.get("AMH_TEST_DATABASE_URL", "postgresql://postgres:postgres@127.0.0.1:5432/postgres")


def _schema_scoped_dsn(admin_url: str, schema: str) -> str:
    """Appends a search_path option to a Postgres connection URL so every
    connection made through it defaults to `schema` — the space in the
    libpq `options` value must be percent-encoded as %20, not the '+'
    urlencode() would produce (form-encoding, not URI encoding; libpq
    doesn't decode '+' as a space)."""
    parsed = urlparse(admin_url)
    options_value = quote(f"-c search_path={schema}", safe="")
    return urlunparse(parsed._replace(query=f"options={options_value}"))

# Fixed test-only tokens — never real secrets, just distinct strings that
# let tests assert agent-vs-operator behavior deterministically. Mirrors
# daemon/api's own test constants (testAgentToken/testOperatorToken).
TEST_AGENT_TOKEN = "test-agent-token"
TEST_OPERATOR_TOKEN = "test-operator-token"

# A fixed, valid (32 raw bytes, base64-encoded) test-only key so the
# daemon's account/credential control-plane routes are enabled in tests
# that use the `daemon` fixture — never a real secret, just deterministic
# test input. See daemon/credentials's doc comment for the real format
# (openssl rand -base64 32).
TEST_CREDENTIAL_KEY = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="


@dataclass(frozen=True)
class DaemonHandle:
    base_url: str
    agent_token: str
    operator_token: str
    mcp_base_url: str


@pytest.fixture(scope="module")
def go_binaries(tmp_path_factory):
    """Builds amh-daemon once per test module."""
    bin_dir = tmp_path_factory.mktemp("bin")
    env = dict(os.environ, GOTOOLCHAIN="local")
    out = str(bin_dir / "amh-daemon")
    subprocess.run(
        ["go", "build", "-o", out, "./daemon/cmd/amh-daemon"],
        cwd=REPO_ROOT, env=env, check=True, capture_output=True, text=True,
    )
    return {"daemon": out}


def _find_free_port() -> int:
    import socket

    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def deterministic_embedding(text: str, dimension: int = 8) -> list[float]:
    """A real, deterministic (not random) embedding stand-in: sha256(text)
    expanded/repeated to `dimension` bytes, mapped to floats in [-1, 1].
    Same text always yields the same vector (so cosine similarity of
    identical text is exactly 1.0), and different text yields a
    different vector — real vector-search behavior over test-fixture
    "embeddings", the same "real protocol, fake remote counterpart"
    pattern this file's own docstring already uses for completions."""
    import hashlib

    digest = hashlib.sha256(text.encode("utf-8")).digest()
    repeated = (digest * (dimension // len(digest) + 1))[:dimension]
    return [(b / 127.5) - 1.0 for b in repeated]


class _FakeModelHandler(BaseHTTPRequestHandler):
    """Implements just enough of an OpenAI-compatible /chat/completions and
    /embeddings endpoint for workflows.goal's real prompts and
    memory/retrieval.py's real embedding calls. Distinguishes
    decompose_goal's call from do_subagent_work's agentic-loop call by
    system-prompt content (both are sent as the first message per
    context/llm.py's _complete_openai_compatible), not by guessing — a
    real model would be told the same way.

    The agentic-loop branch always answers "done" on its first turn —
    this fixture stands in for a model that completes the task
    immediately, not one that actually uses read_file/write_file/etc.
    Real multi-turn tool use is exercised directly against
    harness/agentic_loop.py in test_agentic_loop.py, against a fixture
    that inspects the tool-call sequence rather than a fixed daemon
    account like this one."""

    def log_message(self, format, *args):  # noqa: A002 (stdlib signature) - quiet test output
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length))

        if self.path.endswith("/embeddings"):
            data = [{"embedding": deterministic_embedding(text), "index": i} for i, text in enumerate(body["input"])]
            response = json.dumps({"data": data, "model": body.get("model", "")}).encode("utf-8")
        else:
            messages = body["messages"]
            system_content = messages[0]["content"] if messages and messages[0]["role"] == "system" else ""
            user_content = next(m["content"] for m in reversed(messages) if m["role"] == "user")

            agentic_marker = "Your task:\n\n"
            if "JSON array" in system_content:
                clauses = [c.strip() for c in user_content.split(";") if c.strip()] or [user_content]
                content = json.dumps([{"objective": c} for c in clauses])
            elif agentic_marker in system_content:
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


def register_model_provider_account(daemon, model_server_url: str, provider: str = "test-fake") -> None:
    """Registers model_server_url as a real provider account on the real
    running daemon (the same two calls an operator makes through the
    control-plane UI's Accounts tab) — the same envelope shape
    daemon/inference parses for any openai_compatible provider. Shared by
    fake_model_server and any test that needs its own model-server
    instance with the same registration dance (e.g. one with custom
    tracking behavior a shared fixture shouldn't carry)."""
    envelope = json.dumps({"kind": "openai_compatible", "api_key": "test-fake-key", "base_url": model_server_url})
    create_req = urllib.request.Request(
        f"{daemon.base_url}/v1/accounts",
        data=json.dumps({"provider": provider, "display_name": "fake model server"}).encode(),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {daemon.operator_token}"},
        method="POST",
    )
    with urllib.request.urlopen(create_req, timeout=10) as resp:
        account = json.loads(resp.read())
    # /v1/accounts/{id}/credential takes {"secret": "<opaque string>"} —
    # the envelope JSON travels as that string's value, decrypted and
    # handed back byte-for-byte by daemon/inference, which parses it.
    credential_req = urllib.request.Request(
        f"{daemon.base_url}/v1/accounts/{account['id']}/credential",
        data=json.dumps({"secret": envelope}).encode(),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {daemon.operator_token}"},
        method="POST",
    )
    urllib.request.urlopen(credential_req, timeout=10).close()


@pytest.fixture()
def fake_model_server(daemon, monkeypatch):
    """Starts the fake model HTTP server, registers it as a real
    "test-fake" provider account on the real running daemon, and points
    ADAPTER_MODEL/ADAPTER_PROVIDER at it — the only things workflows.goal's
    from_env() reads from this process's own environment now;
    daemon_api_base_url/agent_token flow explicitly through the workflow
    call graph instead (see workflows/goal.py)."""
    port = _find_free_port()
    server = ThreadingHTTPServer(("127.0.0.1", port), _FakeModelHandler)
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


@pytest.fixture()
def daemon(go_binaries, db_path, tmp_path):
    """Starts amh-daemon pointed at db_path with the two role tokens
    configured, waits for /healthz, tears down. Yields a DaemonHandle
    (base_url, agent_token, operator_token) — real deployments never put
    the operator token in the agent process's own environment; this
    fixture holds both only because tests need to play both roles."""
    api_port = _find_free_port()
    health_port = _find_free_port()
    mcp_port = _find_free_port()
    env = dict(
        os.environ,
        DATABASE_URL=db_path,
        AMH_MIGRATIONS_DIR=MIGRATIONS_DIR,
        AMH_DAEMON_PORT=str(health_port),
        AMH_API_PORT=str(api_port),
        AMH_MCP_PORT=str(mcp_port),
        HABITAT_ROUTINE_TICK_MS="60000",
        AMH_API_AGENT_TOKEN=TEST_AGENT_TOKEN,
        AMH_API_OPERATOR_TOKEN=TEST_OPERATOR_TOKEN,
        AMH_CREDENTIAL_KEY=TEST_CREDENTIAL_KEY,
        AMH_SANDBOX_DIR=str(tmp_path / "computers"),
    )
    proc = subprocess.Popen([go_binaries["daemon"]], cwd=REPO_ROOT, env=env,
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    base_url = f"http://127.0.0.1:{api_port}"
    mcp_base_url = f"http://127.0.0.1:{mcp_port}"
    try:
        _wait_for_health(health_port)
        yield DaemonHandle(base_url=base_url, agent_token=TEST_AGENT_TOKEN, operator_token=TEST_OPERATOR_TOKEN, mcp_base_url=mcp_base_url)
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()


def _wait_for_health(port: int, timeout: float = 10.0) -> None:
    deadline = time.time() + timeout
    last_error = None
    while time.time() < deadline:
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=1)
            return
        except (urllib.error.URLError, ConnectionError) as e:
            last_error = e
            time.sleep(0.1)
    raise TimeoutError(f"daemon did not become healthy within {timeout}s: {last_error}")


@pytest.fixture()
def db_path():
    """Despite the historical name (SQLite-era: a file path), this yields a
    PostgreSQL connection URL scoped to a fresh, isolated schema — kept as
    `db_path` rather than renamed, since every test in this suite already
    takes it as a fixture parameter by that name."""
    from workflows import ontology

    schema = f"test_{os.getpid()}_{secrets.token_hex(8)}"
    admin = psycopg.connect(TEST_POSTGRES_ADMIN_URL, autocommit=True)
    admin.execute(f'CREATE SCHEMA "{schema}"')
    admin.close()

    dsn = _schema_scoped_dsn(TEST_POSTGRES_ADMIN_URL, schema)
    ontology.apply_migrations(dsn, MIGRATIONS_DIR)

    try:
        yield dsn
    finally:
        cleanup = psycopg.connect(TEST_POSTGRES_ADMIN_URL, autocommit=True)
        cleanup.execute(f'DROP SCHEMA IF EXISTS "{schema}" CASCADE')
        cleanup.close()
