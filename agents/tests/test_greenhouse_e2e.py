"""End-to-end test for the V0 walking-skeleton scenario (Artifact H, steps
1-4), across real process boundaries: a Python DBOS workflow orchestrates
decomposition, isolated sub-agent execution, and context compaction, then
drives a genuine SSH actuation against a Go-built device simulator via the
amh-actuate CLI bridge — the same actuation kernel already unit-tested in
daemon/actuation/actuate_test.go, now exercised from the other side of the
process boundary it will eventually cross over a real RPC bridge.

Requires a working Go toolchain (go build) and node/npx are NOT needed
here — only go. Skipped if `go` is unavailable.
"""

from __future__ import annotations

import os
import shutil
import sqlite3
import subprocess
import sys
import textwrap
import time
import uuid

import pytest

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
MIGRATIONS_DIR = os.path.join(REPO_ROOT, "store", "migrations")

pytestmark = pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain not available")


@pytest.fixture(scope="module")
def go_binaries(tmp_path_factory):
    """Builds amh-fake-device and amh-actuate once per test module."""
    bin_dir = tmp_path_factory.mktemp("bin")
    env = dict(os.environ, GOTOOLCHAIN="local")
    for name, pkg in [
        ("amh-fake-device", "./daemon/cmd/amh-fake-device"),
        ("amh-actuate", "./daemon/cmd/amh-actuate"),
    ]:
        out = str(bin_dir / name)
        subprocess.run(
            ["go", "build", "-o", out, pkg],
            cwd=REPO_ROOT, env=env, check=True, capture_output=True, text=True,
        )
    return {
        "fake_device": str(bin_dir / "amh-fake-device"),
        "actuate": str(bin_dir / "amh-actuate"),
    }


@pytest.fixture()
def fake_device(go_binaries):
    """Starts the SSH device simulator, yields (host, port), tears it down."""
    proc = subprocess.Popen(
        [go_binaries["fake_device"], "--initial-open-pct", "40"],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
    )
    try:
        listen_line = proc.stdout.readline()
        assert listen_line.startswith("LISTEN "), f"unexpected fake-device output: {listen_line!r}"
        addr = listen_line.strip().split(" ", 1)[1]
        host, port = addr.rsplit(":", 1)
        assert proc.stdout.readline().strip() == "READY"
        yield host, int(port)
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()


@pytest.fixture()
def db_path(tmp_path):
    from workflows import ontology

    path = str(tmp_path / "amh.db")
    ontology.apply_migrations(path, MIGRATIONS_DIR)
    return path


def seed_vent(db_path: str) -> None:
    conn = sqlite3.connect(db_path)
    conn.execute("INSERT INTO connector (id, type, auth) VALUES ('greenhouse-vent', 'ssh', 'none')")
    conn.execute("INSERT INTO device (id, kind, connector_id) VALUES ('vent-actuator', 'vent', 'greenhouse-vent')")
    conn.execute(
        """INSERT INTO device_action (id, device_id, name, reversible, inverse_template, verified_at)
           VALUES ('vent-actuator.set_open_pct', 'vent-actuator', 'set_open_pct', 1,
                   '{"shell_template": "vent-ctl set-open-pct {{prior}}"}',
                   strftime('%Y-%m-%dT%H:%M:%fZ','now'))"""
    )
    conn.commit()
    conn.close()


def test_greenhouse_scenario_steps_1_to_4_end_to_end(go_binaries, fake_device, db_path):
    from dbos import DBOS

    from workflows.greenhouse import run_greenhouse_scenario
    from workflows.runtime import init_dbos

    host, port = fake_device
    seed_vent(db_path)

    init_dbos("amh-greenhouse-e2e", db_path)
    DBOS.launch()
    try:
        goal_id = str(uuid.uuid4())
        result = run_greenhouse_scenario(
            goal_id,
            "monitor greenhouse temperature; open vent on threshold",
            db_path,
            MIGRATIONS_DIR,
            go_binaries["actuate"],
            "vent-actuator.set_open_pct",
            host, port,
            forward="vent-ctl set-open-pct 60",
            read_state="vent-ctl get-open-pct",
        )
    finally:
        DBOS.destroy()

    # Step 1-2: decomposition + subagents completed
    assert "monitor greenhouse temperature" in result["summary"]
    assert "open vent on threshold" in result["summary"]

    # Step 3: compaction fired under sustained polling
    assert result["compaction"]["compacted"] is True
    assert result["compaction"]["turns_compacted"] > 0

    # Step 4: real autonomous actuation, no ApprovalGate needed
    assert result["actuation_result"] == "ok"

    conn = sqlite3.connect(db_path)
    (status,) = conn.execute("SELECT status FROM goal WHERE id = ?", (goal_id,)).fetchone()
    assert status == "done"

    forward, inverse, outcome = conn.execute(
        "SELECT forward_payload, inverse_payload, outcome FROM device_effect WHERE device_action_id = ?",
        ("vent-actuator.set_open_pct",),
    ).fetchone()
    assert outcome == "success"
    assert forward == '{"shell":"vent-ctl set-open-pct 60"}'
    # The recorded inverse must reflect the device's actual PRIOR state (40),
    # proving the actuation kernel really read it from the live device over
    # SSH before actuating — not a hardcoded or assumed value.
    assert inverse == '{"shell":"vent-ctl set-open-pct 40"}'
    conn.close()


def test_greenhouse_scenario_survives_process_restart(go_binaries, fake_device, db_path):
    """Starts run_greenhouse_scenario asynchronously, crashes the Python
    process with os._exit before it reaches step 4, then a second,
    independent Python process resumes via DBOS.launch() alone (never
    re-invoking the workflow function) and completes it — including
    running the real SSH actuation against the still-running fake device.
    The device process survives the Python-side crash exactly as a real
    physical device would survive the daemon restarting.
    """
    host, port = fake_device
    seed_vent(db_path)

    goal_id = str(uuid.uuid4())
    workflow_id = f"greenhouse-{goal_id}"
    goal_text = "monitor greenhouse temperature; open vent on threshold"

    start_script = textwrap.dedent(f"""
        import sys
        sys.path.insert(0, {(REPO_ROOT + "/agents")!r})
        from dbos import DBOS, SetWorkflowID
        from workflows.greenhouse import run_greenhouse_scenario
        from workflows.runtime import init_dbos

        init_dbos("amh-greenhouse-e2e", {db_path!r})
        DBOS.launch()
        with SetWorkflowID({workflow_id!r}):
            DBOS.start_workflow(
                run_greenhouse_scenario,
                {goal_id!r}, {goal_text!r}, {db_path!r}, {MIGRATIONS_DIR!r},
                {go_binaries["actuate"]!r}, "vent-actuator.set_open_pct",
                {host!r}, {port!r}, "vent-ctl set-open-pct 60", "vent-ctl get-open-pct",
            )
        import os
        os._exit(1)  # crash before get_result() — simulates the daemon dying mid-flight
    """)
    start_result = subprocess.run(
        [sys.executable, "-c", start_script],
        cwd=os.path.join(REPO_ROOT, "agents"),
        capture_output=True, text=True, timeout=30,
    )
    assert start_result.returncode == 1, start_result.stderr

    resume_script = textwrap.dedent(f"""
        import sys
        sys.path.insert(0, {(REPO_ROOT + "/agents")!r})
        from dbos import DBOS
        from workflows.greenhouse import run_greenhouse_scenario  # noqa: F401 (registers workflow)
        from workflows.runtime import init_dbos

        init_dbos("amh-greenhouse-e2e", {db_path!r})
        DBOS.launch()  # triggers automatic recovery of the pending workflow
        handle = DBOS.retrieve_workflow({workflow_id!r})
        result = handle.get_result()
        print("ACTUATION:" + result["actuation_result"])
        DBOS.destroy()
    """)
    resume_result = subprocess.run(
        [sys.executable, "-c", resume_script],
        cwd=os.path.join(REPO_ROOT, "agents"),
        capture_output=True, text=True, timeout=30,
    )
    assert resume_result.returncode == 0, resume_result.stderr
    assert "ACTUATION:ok" in resume_result.stdout

    conn = sqlite3.connect(db_path)
    (status,) = conn.execute("SELECT status FROM goal WHERE id = ?", (goal_id,)).fetchone()
    assert status == "done"
    (n,) = conn.execute(
        "SELECT COUNT(*) FROM device_effect WHERE device_action_id = ?",
        ("vent-actuator.set_open_pct",),
    ).fetchone()
    assert n == 1  # exactly one actuation — no double-execution across the crash/resume
    conn.close()
