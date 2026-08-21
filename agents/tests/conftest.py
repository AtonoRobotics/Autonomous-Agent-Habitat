"""Shared fixtures for end-to-end tests that need a real Go daemon and a
real SSH device simulator — used by test_greenhouse_e2e.py and
test_approval_e2e.py. See daemon/cmd/amh-daemon and
daemon/cmd/amh-fake-device.
"""

from __future__ import annotations

import os
import subprocess
import time
import urllib.error
import urllib.request
from dataclasses import dataclass

import pytest

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
MIGRATIONS_DIR = os.path.join(REPO_ROOT, "store", "migrations")

# Fixed test-only tokens — never real secrets, just distinct strings that
# let tests assert agent-vs-operator behavior deterministically. Mirrors
# daemon/api's own test constants (testAgentToken/testOperatorToken).
TEST_AGENT_TOKEN = "test-agent-token"
TEST_OPERATOR_TOKEN = "test-operator-token"


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


@pytest.fixture()
def daemon(go_binaries, db_path):
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
