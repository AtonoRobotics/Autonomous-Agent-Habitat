"""Device actuation step: bridges a DBOS workflow to the Go daemon's
device-actuation kernel (daemon/actuation), which owns reversibility
gating, the ApprovalGate, and connector I/O (§12, §14.6, Artifact F).

V0 bridge: subprocess call to the amh-actuate CLI (daemon/cmd/amh-actuate),
not a network RPC — see that command's doc comment for why. Wrapped as a
@DBOS.step() so a crash between "actuation happened" and "workflow
recorded it happened" cannot occur silently: the step either completes
(and DBOS durably records that) or the workflow sees the failure and can
retry/escalate, exactly as any other step.
"""

from __future__ import annotations

import json
import subprocess

from dbos import DBOS

from context.observability import tool_call_span


class ActuationError(Exception):
    pass


@DBOS.step()
def actuate_device(
    amh_actuate_bin: str,
    db_path: str,
    migrations_dir: str,
    device_action_id: str,
    host: str,
    port: int,
    forward: str,
    read_state: str = "",
    ticket_id: str = "",
    insecure_skip_host_key_verify: bool = False,
) -> str:
    """Runs amh-actuate for one device action and returns its result
    string. Raises ActuationError (with the CLI's structured error
    message) on any failure — including the fail-closed cases enforced by
    daemon/actuation.Execute (no verified inverse + no approved SafetyCase
    + no approved ticket)."""
    args = [
        amh_actuate_bin,
        "--db", db_path,
        "--migrations", migrations_dir,
        "--device-action-id", device_action_id,
        "--host", host,
        "--port", str(port),
        "--forward", forward,
    ]
    if read_state:
        args += ["--read-state", read_state]
    if ticket_id:
        args += ["--ticket-id", ticket_id]
    if insecure_skip_host_key_verify:
        args += ["--insecure-skip-host-key-verify"]

    with tool_call_span(f"device_action:{device_action_id}", **{"amh.device.host": host}) as span:
        proc = subprocess.run(args, capture_output=True, text=True, timeout=30)
        try:
            payload = json.loads(proc.stdout)
        except json.JSONDecodeError:
            span.set_attribute("error.type", "non_json_output")
            raise ActuationError(f"amh-actuate produced non-JSON output: {proc.stdout!r} {proc.stderr!r}")

        if payload.get("error"):
            span.set_attribute("error.type", "actuation_error")
            span.set_attribute("error.message", payload["error"])
            raise ActuationError(payload["error"])
        return payload["result"]
