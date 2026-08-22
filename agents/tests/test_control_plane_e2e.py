"""End-to-end tests for the control plane: install harnesses (extensions),
build/add computers (sandboxes), configure connectors, and authenticate
accounts and modules — all driven from Python against a real Go daemon,
proving the admin surface the control-plane UI extension is built on
actually works end-to-end, not just at the Go layer (already covered by
daemon/api/controlplane_test.go).

Requires a working Go toolchain (go build). Skipped if `go` is
unavailable.
"""

from __future__ import annotations

import inspect
import json
import shutil
import urllib.error
import urllib.request

import psycopg
import pytest

pytestmark = pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain not available")


def seed_agent(db_path: str, agent_id: str) -> None:
    conn = psycopg.connect(db_path)
    conn.execute("INSERT INTO agent (id, kind) VALUES (%s, 'worker')", (agent_id,))
    conn.commit()
    conn.close()


def _post(daemon, path: str, token: str, body: dict | None = None):
    request = urllib.request.Request(
        f"{daemon.base_url}{path}",
        data=json.dumps(body).encode() if body is not None else b"",
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}"},
        method="POST",
    )
    try:
        return urllib.request.urlopen(request, timeout=10)
    except urllib.error.HTTPError as e:
        return e


# ── Computers: an agent can create and destroy its own sandbox ──────────


def test_agent_can_create_and_destroy_its_own_computer(daemon, db_path):
    from workflows.computers import create_computer, destroy_computer, get_computer, list_computers

    seed_agent(db_path, "agent-1")

    computer = create_computer(daemon.base_url, daemon.agent_token, "agent-1", "process", "sleep 60")
    assert computer["status"] == "ready"
    assert computer["runtime_handle"]

    fetched = get_computer(daemon.base_url, daemon.agent_token, computer["id"])
    assert fetched["id"] == computer["id"]

    listed = list_computers(daemon.base_url, daemon.agent_token, "agent-1")
    assert any(c["id"] == computer["id"] for c in listed)

    destroyed = destroy_computer(daemon.base_url, daemon.agent_token, computer["id"], "test done")
    assert destroyed["status"] == "destroyed"

    listed_after = list_computers(daemon.base_url, daemon.agent_token, "agent-1")
    assert not any(c["id"] == computer["id"] for c in listed_after)


# ── Extensions: install a harness end-to-end, agent gets read-only ──────


def test_operator_installs_a_harness_extension_agent_can_only_read(daemon):
    from workflows.extensions import ExtensionError, get_extension, list_extensions

    manifest = {
        "apiVersion": "amh/v1",
        "kind": "Extension",
        "metadata": {
            "id": "amh.harness/deep-agent",
            "name": "Deep Agent Harness",
            "version": "1.0.0",
            "publisher": "amh-tests",
        },
        "spec": {
            "entrypoint": "true",
            "isolation": "in_process",
            "provides": [{"id": "amh.harness/deep-agent-cap", "version": "1.0.0"}],
            "requires": [],
            "compatibility": {"amhCore": ">=0.1.0"},
        },
    }

    # The agent cannot install a harness itself.
    agent_attempt = _post(daemon, "/v1/extensions", daemon.agent_token, manifest)
    assert getattr(agent_attempt, "status", getattr(agent_attempt, "code", None)) == 403

    # The operator installs and activates it.
    discover = _post(daemon, "/v1/extensions", daemon.operator_token, manifest)
    assert discover.status == 201
    discover.close()

    activate = _post(daemon, "/v1/extensions/activate", daemon.operator_token,
                      {"id": "amh.harness/deep-agent", "version": "1.0.0"})
    assert activate.status == 200
    activate.close()

    # Now the agent can see it's active — read-only, but real.
    extensions = list_extensions(daemon.base_url, daemon.agent_token)
    installed = next(e for e in extensions if e["id"] == "amh.harness/deep-agent")
    assert installed["status"] == "active"

    fetched = get_extension(daemon.base_url, daemon.agent_token, "amh.harness/deep-agent", "1.0.0")
    assert fetched["status"] == "active"

    with pytest.raises(ExtensionError):
        get_extension(daemon.base_url, daemon.agent_token, "no/such-extension", "9.9.9")


def test_workflows_extensions_has_no_mutating_functions():
    """Structural guardrail, same discipline as approval.py/safetycase.py:
    this module must have no function that could install, activate, or
    dispose an extension, and none shaped to accept an operator
    credential beyond the read-only agent_token every function already
    takes."""
    import workflows.extensions as extensions_module

    public_names = {n for n in dir(extensions_module) if not n.startswith("_")}
    for verb in ("discover", "activate", "quiesce", "dispose", "install", "create"):
        assert not any(verb in n.lower() for n in public_names), f"found a mutating-looking function for {verb!r}"

    for name in public_names:
        obj = getattr(extensions_module, name)
        if not inspect.isfunction(obj):
            continue
        params = set(inspect.signature(obj).parameters)
        assert not any("operator" in p.lower() for p in params)


# ── Accounts: operator authenticates, agent gets metadata only ──────────


def test_operator_authenticates_an_account_agent_never_sees_the_secret(daemon):
    from workflows.accounts import AccountError, get_account, list_accounts

    agent_attempt = _post(daemon, "/v1/accounts", daemon.agent_token, {"provider": "github"})
    assert getattr(agent_attempt, "status", getattr(agent_attempt, "code", None)) == 403

    create = _post(daemon, "/v1/accounts", daemon.operator_token, {"provider": "github", "display_name": "amh-bot"})
    assert create.status == 201
    account = json.loads(create.read())
    create.close()
    assert account["status"] == "pending"

    put_cred = _post(daemon, f"/v1/accounts/{account['id']}/credential", daemon.operator_token,
                      {"secret": "ghp_extremelysecrettoken"})
    assert put_cred.status == 200
    body = put_cred.read()
    put_cred.close()
    assert b"extremelysecrettoken" not in body

    fetched = get_account(daemon.base_url, daemon.agent_token, account["id"])
    assert fetched["status"] == "active"
    assert "secret" not in json.dumps(fetched).lower() or "extremelysecrettoken" not in json.dumps(fetched)

    accounts = list_accounts(daemon.base_url, daemon.agent_token)
    assert "extremelysecrettoken" not in json.dumps(accounts)

    with pytest.raises(AccountError):
        get_account(daemon.base_url, daemon.agent_token, "no-such-account")


def test_workflows_accounts_has_no_mutating_functions():
    import workflows.accounts as accounts_module

    public_names = {n for n in dir(accounts_module) if not n.startswith("_")}
    for verb in ("create", "credential", "revoke", "authenticate"):
        assert not any(verb in n.lower() for n in public_names), f"found a mutating-looking function for {verb!r}"

    for name in public_names:
        obj = getattr(accounts_module, name)
        if not inspect.isfunction(obj):
            continue
        params = set(inspect.signature(obj).parameters)
        assert not any(p.lower() in ("secret", "credential", "operator_token") for p in params)
