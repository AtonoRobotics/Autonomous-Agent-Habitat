"""End-to-end test for the greenhouse scenario (Artifact H, steps
1-4), across real process boundaries: a Python DBOS workflow orchestrates
decomposition, isolated sub-agent execution, and context compaction, then
drives a genuine SSH actuation through the Go daemon's persistent
actuation API (daemon/api) against a Go-built device simulator — the same
actuation kernel already unit-tested in daemon/actuation/actuate_test.go
and daemon/api/api_test.go, now exercised from the Python side of the
process boundary it actually runs across in a real deployment (daemon and
device stay up independently of the Python agent process).

Requires a working Go toolchain (go build). Skipped if `go` is
unavailable.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import textwrap
import uuid

import psycopg
import pytest

from conftest import REPO_ROOT, write_ephemeral_client_key  # noqa: F401 (fixtures via conftest autouse)

pytestmark = pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain not available")


def seed_vent(db_path: str, host: str, port: int, host_key_authorized_key: str, tmp_path) -> None:
    config = {
        "host": host,
        "port": port,
        "user": "amh",
        "private_key_path": write_ephemeral_client_key(tmp_path),
        "host_key_authorized_key": host_key_authorized_key,
    }
    conn = psycopg.connect(db_path)
    conn.execute(
        "INSERT INTO connector (id, type, auth, config) VALUES ('greenhouse-vent', 'ssh', 'none', %s)",
        (json.dumps(config),),
    )
    conn.execute("INSERT INTO device (id, kind, connector_id) VALUES ('vent-actuator', 'vent', 'greenhouse-vent')")
    conn.execute(
        """INSERT INTO device_action (id, device_id, name, reversible, forward_template, read_state_template, inverse_template, verified_at)
           VALUES ('vent-actuator.set_open_pct', 'vent-actuator', 'set_open_pct', 1,
                   '{"shell_template": "vent-ctl set-open-pct {{open_pct}}"}',
                   '{"shell_template": "vent-ctl get-open-pct"}',
                   '{"shell_template": "vent-ctl set-open-pct {{prior}}"}',
                   iso8601_now())"""
    )
    conn.commit()
    conn.close()


def test_greenhouse_scenario_steps_1_to_4_end_to_end(fake_device, daemon, db_path, tmp_path, fake_model_server):
    from dbos import DBOS

    from workflows.greenhouse import run_greenhouse_scenario
    from workflows.runtime import init_dbos

    host, port, host_key_authorized_key = fake_device
    seed_vent(db_path, host, port, host_key_authorized_key, tmp_path)

    init_dbos("amh-greenhouse-e2e", db_path)
    DBOS.launch()
    try:
        goal_id = str(uuid.uuid4())
        result = run_greenhouse_scenario(
            goal_id,
            "monitor greenhouse temperature; open vent on threshold",
            db_path,
            daemon.base_url,
            daemon.agent_token,
            "vent-actuator.set_open_pct",
            {"open_pct": "60"},
        )
    finally:
        DBOS.destroy()

    # Step 1-2: decomposition + subagents completed
    assert "monitor greenhouse temperature" in result["summary"]
    assert "open vent on threshold" in result["summary"]

    # Step 3: compaction fired under sustained polling
    assert result["compaction"]["compacted"] is True
    assert result["compaction"]["turns_compacted"] > 0

    # Step 4: real autonomous actuation through the daemon's API, no ApprovalGate needed
    assert result["actuation_result"] == "ok"

    conn = psycopg.connect(db_path)
    (status,) = conn.execute("SELECT status FROM goal WHERE id = %s", (goal_id,)).fetchone()
    assert status == "done"

    forward, inverse, outcome = conn.execute(
        "SELECT forward_payload, inverse_payload, outcome FROM device_effect WHERE device_action_id = %s",
        ("vent-actuator.set_open_pct",),
    ).fetchone()
    assert outcome == "success"
    # psycopg auto-decodes a JSON column into a Python dict/list already —
    # no json.loads() needed (unlike sqlite3, which returns raw text).
    assert forward == {"shell": "vent-ctl set-open-pct 60"}
    # The recorded inverse must reflect the device's actual PRIOR state (40),
    # proving the actuation kernel really read it from the live device over
    # SSH before actuating — not a hardcoded or assumed value.
    assert inverse == {"shell": "vent-ctl set-open-pct 40"}
    conn.close()


def test_greenhouse_scenario_survives_process_restart(fake_device, daemon, db_path, tmp_path, fake_model_server):
    """Starts run_greenhouse_scenario asynchronously, crashes the Python
    process with os._exit before it reaches step 4, then a second,
    independent Python process resumes via DBOS.launch() alone (never
    re-invoking the workflow function) and completes it — including
    running the real SSH actuation through the daemon's API against the
    still-running fake device. Both the daemon and the device survive the
    Python-side crash exactly as they would survive the agent process
    restarting in a real deployment.
    """
    host, port, host_key_authorized_key = fake_device
    seed_vent(db_path, host, port, host_key_authorized_key, tmp_path)

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
                {goal_id!r}, {goal_text!r}, {db_path!r}, {daemon.base_url!r}, {daemon.agent_token!r},
                "vent-actuator.set_open_pct",
                {{"open_pct": "60"}},
            )
        import os
        os._exit(1)  # crash before get_result() — simulates the agent process dying mid-flight
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

    conn = psycopg.connect(db_path)
    (status,) = conn.execute("SELECT status FROM goal WHERE id = %s", (goal_id,)).fetchone()
    assert status == "done"
    (n,) = conn.execute(
        "SELECT COUNT(*) FROM device_effect WHERE device_action_id = %s",
        ("vent-actuator.set_open_pct",),
    ).fetchone()
    assert n == 1  # exactly one actuation — no double-execution across the crash/resume
    conn.close()
