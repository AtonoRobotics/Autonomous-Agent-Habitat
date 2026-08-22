"""Shared fixtures for end-to-end tests that need a real Go daemon and a
real SSH device simulator — used by test_greenhouse_e2e.py and
test_approval_e2e.py. See daemon/cmd/amh-daemon and
daemon/cmd/amh-fake-device.

Also provides fake_model_server: a real local HTTP server standing in for
a model provider, registered as a real account against a real running
daemon (daemon/inference, daemon/credentials) exactly the way an operator
would register a real provider — so workflows.goal's real model calls
travel the actual path (agent token -> daemon -> provider account
credential -> HTTP call) end to end, not a shortcut through the agent
process's own environment. This is the same "real protocol, fake remote
counterpart" pattern amh-fake-device already uses for SSH: the
decomposition/completion logic this server returns is deliberately
test-fixture logic, standing in for what a real model would answer for
these specific deterministic test inputs — it does not reintroduce
placeholder logic into workflows/goal.py or daemon/inference itself.
"""

from __future__ import annotations

import json
import os
import subprocess
import threading
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
MIGRATIONS_DIR = os.path.join(REPO_ROOT, "store", "migrations")

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


@pytest.fixture(scope="module")
def go_binaries(tmp_path_factory):
    """Builds amh-fake-device and amh-daemon once per test module."""
    bin_dir = tmp_path_factory.mktemp("bin")
    env = dict(os.environ, GOTOOLCHAIN="local")
    for name, pkg in [
        ("amh-fake-device", "./daemon/cmd/amh-fake-device"),
        ("amh-daemon", "./daemon/cmd/amh-daemon"),
    ]:
        out = str(bin_dir / name)
        subprocess.run(
            ["go", "build", "-o", out, pkg],
            cwd=REPO_ROOT, env=env, check=True, capture_output=True, text=True,
        )
    return {
        "fake_device": str(bin_dir / "amh-fake-device"),
        "daemon": str(bin_dir / "amh-daemon"),
    }


@pytest.fixture()
def fake_device(go_binaries):
    """Starts an SSH device simulator, yields (host, port, host_key_authorized_key)."""
    proc = subprocess.Popen(
        [go_binaries["fake_device"], "--initial-open-pct", "40"],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
    )
    try:
        listen_line = proc.stdout.readline()
        assert listen_line.startswith("LISTEN "), f"unexpected fake-device output: {listen_line!r}"
        addr = listen_line.strip().split(" ", 1)[1]
        host, port = addr.rsplit(":", 1)

        hostkey_line = proc.stdout.readline()
        assert hostkey_line.startswith("HOSTKEY "), f"unexpected fake-device output: {hostkey_line!r}"
        host_key_authorized_key = hostkey_line.strip().split(" ", 1)[1]

        assert proc.stdout.readline().strip() == "READY"
        yield host, int(port), host_key_authorized_key
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()


def _find_free_port() -> int:
    import socket

    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class _FakeModelHandler(BaseHTTPRequestHandler):
    """Implements just enough of an OpenAI-compatible /chat/completions
    endpoint for workflows.goal's real prompts. Distinguishes
    decompose_goal's call from do_subagent_work's call by system-prompt
    content (both are sent as the first message per context/llm.py's
    _complete_openai_compatible), not by guessing — a real model would be
    told the same way."""

    def log_message(self, format, *args):  # noqa: A002 (stdlib signature) - quiet test output
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length))
        messages = body["messages"]
        system_content = messages[0]["content"] if messages and messages[0]["role"] == "system" else ""
        user_content = next(m["content"] for m in reversed(messages) if m["role"] == "user")

        if "JSON array" in system_content:
            clauses = [c.strip() for c in user_content.split(";") if c.strip()] or [user_content]
            content = json.dumps([{"objective": c} for c in clauses])
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
    env = dict(
        os.environ,
        DATABASE_URL=f"sqlite:{db_path}",
        AMH_MIGRATIONS_DIR=MIGRATIONS_DIR,
        AMH_DAEMON_PORT=str(health_port),
        AMH_API_PORT=str(api_port),
        HABITAT_ROUTINE_TICK_MS="60000",
        AMH_API_AGENT_TOKEN=TEST_AGENT_TOKEN,
        AMH_API_OPERATOR_TOKEN=TEST_OPERATOR_TOKEN,
        AMH_CREDENTIAL_KEY=TEST_CREDENTIAL_KEY,
        AMH_SANDBOX_DIR=str(tmp_path / "computers"),
    )
    proc = subprocess.Popen([go_binaries["daemon"]], cwd=REPO_ROOT, env=env,
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    base_url = f"http://127.0.0.1:{api_port}"
    try:
        _wait_for_health(health_port)
        yield DaemonHandle(base_url=base_url, agent_token=TEST_AGENT_TOKEN, operator_token=TEST_OPERATOR_TOKEN)
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
def db_path(tmp_path):
    from workflows import ontology

    path = str(tmp_path / "amh.db")
    ontology.apply_migrations(path, MIGRATIONS_DIR)
    return path


def write_ephemeral_client_key(tmp_path) -> str:
    """Generates a throwaway RSA key and writes it as a PEM file — the
    fake device accepts any client key, so identity doesn't matter here,
    only that the daemon's connector config points at a real, readable
    private key file, matching how a real deployment's secret would be
    referenced by path rather than embedded in the database."""
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric import rsa

    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    pem = key.private_bytes(
        encoding=serialization.Encoding.PEM,
        format=serialization.PrivateFormat.TraditionalOpenSSL,
        encryption_algorithm=serialization.NoEncryption(),
    )
    path = str(tmp_path / "client_key.pem")
    with open(path, "wb") as f:
        f.write(pem)
    return path
