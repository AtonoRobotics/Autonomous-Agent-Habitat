"""End-to-end interop test for daemon/mcp — the strongest verification
available for a hand-rolled protocol implementation: not a Go-side unit
test asserting our own wire shapes are self-consistent, but a real
client, from the official MCP Python SDK (the same package
agents/harness/mcp_client.py already depends on for the stdio direction),
speaking Streamable HTTP to a real running amh-daemon binary and getting
back real tool results from a real SSH-actuated device.

Requires a working Go toolchain. Skipped if `go` is unavailable.
"""

from __future__ import annotations

import json
import shutil

import httpx2
import psycopg
import pytest
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client

from conftest import write_ephemeral_client_key

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


@pytest.mark.asyncio
async def test_official_mcp_client_calls_actuate_device_over_streamable_http(fake_device, daemon, db_path, tmp_path):
    host, port, host_key_authorized_key = fake_device
    seed_vent(db_path, host, port, host_key_authorized_key, tmp_path)

    http_client = httpx2.AsyncClient(headers={"Authorization": f"Bearer {daemon.agent_token}"})
    async with streamable_http_client(daemon.mcp_base_url + "/mcp", http_client=http_client) as (read, write):
        async with ClientSession(read, write) as session:
            init_result = await session.initialize()
            assert init_result.protocol_version == "2025-06-18"

            tools_result = await session.list_tools()
            names = {t.name for t in tools_result.tools}
            assert {"actuate_device", "request_approval", "check_approval"} <= names

            call_result = await session.call_tool(
                "actuate_device",
                {"device_action_id": "vent-actuator.set_open_pct", "params": {"open_pct": "60"}},
            )
            assert call_result.is_error is not True
            texts = [c.text for c in call_result.content if c.type == "text"]
            assert texts == ["ok"]

    # The real device really moved — proven by inspecting the daemon's
    # own durable record of the effect, not just the MCP response.
    conn = psycopg.connect(db_path)
    inverse, outcome = conn.execute(
        "SELECT inverse_payload, outcome FROM device_effect WHERE device_action_id = %s",
        ("vent-actuator.set_open_pct",),
    ).fetchone()
    assert outcome == "success"
    assert inverse == {"shell": "vent-ctl set-open-pct 40"}
    conn.close()


@pytest.mark.asyncio
async def test_official_mcp_client_sees_tool_error_not_protocol_error(daemon, db_path):
    """An actuation failure (no device_action registered at all) must
    surface as a tool-level error the client can inspect
    (call_result.isError), not as an MCP protocol-level exception — this
    is the real MCP SDK enforcing that distinction from the other side of
    the wire, not just our own server-side assumption about it."""
    http_client = httpx2.AsyncClient(headers={"Authorization": f"Bearer {daemon.agent_token}"})
    async with streamable_http_client(daemon.mcp_base_url + "/mcp", http_client=http_client) as (read, write):
        async with ClientSession(read, write) as session:
            await session.initialize()
            call_result = await session.call_tool(
                "actuate_device",
                {"device_action_id": "does-not-exist", "params": {}},
            )
            assert call_result.is_error is True
