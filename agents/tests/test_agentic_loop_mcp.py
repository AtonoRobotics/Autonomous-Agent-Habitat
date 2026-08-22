"""Real MCP tool integration for harness/agentic_loop.py, against the same
official @modelcontextprotocol/server-filesystem reference server
test_mcp_client.py already uses — a genuine protocol round-trip (stdio
transport, JSON-RPC list_tools/call_tool), not a mock of the MCP wire
protocol. See that file's docstring for the fixture-install prerequisite.
"""

from __future__ import annotations

import json
import os
import shutil
from http.server import BaseHTTPRequestHandler, HTTPServer
from threading import Thread

import pytest

from context.llm import ModelClient
from harness.agentic_loop import MCPServerSpec, mcp_servers_from_env, run_agentic_loop
from harness.vfs import VFS

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SERVER_ENTRYPOINT = os.path.join(
    REPO_ROOT, "fixtures", "mcp-servers", "node_modules",
    "@modelcontextprotocol", "server-filesystem", "dist", "index.js",
)

pytestmark = pytest.mark.skipif(
    shutil.which("node") is None or not os.path.exists(SERVER_ENTRYPOINT),
    reason="node unavailable or fixtures/mcp-servers/ not installed (run `npm install` there)",
)


class _ScriptedDaemon(BaseHTTPRequestHandler):
    responses: list[str] = []
    call_count = 0
    last_system_prompt = ""

    def log_message(self, format, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length))
        cls = type(self)
        cls.last_system_prompt = body.get("system", "")
        text = cls.responses[cls.call_count]
        cls.call_count += 1
        response = json.dumps({"text": text}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(response)


@pytest.fixture()
def scripted_daemon():
    _ScriptedDaemon.responses = []
    _ScriptedDaemon.call_count = 0
    server = HTTPServer(("127.0.0.1", 0), _ScriptedDaemon)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        thread.join(timeout=5)


def _client(scripted_daemon) -> ModelClient:
    return ModelClient(daemon_api_base_url=scripted_daemon, agent_token="tok", model="test-model")


def test_loop_calls_a_real_mcp_tool_and_sees_its_real_effect(scripted_daemon, tmp_path):
    mcp_root = tmp_path / "mcp-root"
    mcp_root.mkdir()
    mcp_server = MCPServerSpec(name="fs", command="node", args=[SERVER_ENTRYPOINT, str(mcp_root)])

    write_path = str(mcp_root / "hello.txt")
    _ScriptedDaemon.responses = [
        json.dumps({"tool": "mcp__fs__write_file", "args": {"path": write_path, "content": "hello from the loop via MCP"}}),
        json.dumps({"tool": "mcp__fs__read_text_file", "args": {"path": write_path}}),
        json.dumps({"tool": "done", "result": "wrote and re-read the file over MCP"}),
    ]
    vfs = VFS(str(tmp_path / "vfs-root"))

    result = run_agentic_loop("write a file via the real MCP server", vfs, _client(scripted_daemon), mcp_servers=[mcp_server])

    assert result.result == "wrote and re-read the file over MCP"
    assert result.turns_used == 3
    # The real MCP server process really wrote the file to disk.
    assert (mcp_root / "hello.txt").read_text() == "hello from the loop via MCP"


def test_system_prompt_lists_the_real_mcp_tool_catalog(scripted_daemon, tmp_path):
    mcp_root = tmp_path / "mcp-root"
    mcp_root.mkdir()
    mcp_server = MCPServerSpec(name="fs", command="node", args=[SERVER_ENTRYPOINT, str(mcp_root)])

    _ScriptedDaemon.responses = [json.dumps({"tool": "done", "result": "looked at the tools"})]
    vfs = VFS(str(tmp_path / "vfs-root"))

    run_agentic_loop("just look", vfs, _client(scripted_daemon), mcp_servers=[mcp_server])

    # The tool catalog in the system prompt was built from the real
    # server's real list_tools() response, not a hand-written stand-in.
    assert "mcp__fs__write_file" in _ScriptedDaemon.last_system_prompt
    assert "mcp__fs__read_text_file" in _ScriptedDaemon.last_system_prompt


def test_mcp_tool_error_is_fed_back_not_raised(scripted_daemon, tmp_path):
    mcp_root = tmp_path / "mcp-root"
    mcp_root.mkdir()
    mcp_server = MCPServerSpec(name="fs", command="node", args=[SERVER_ENTRYPOINT, str(mcp_root)])

    _ScriptedDaemon.responses = [
        json.dumps({"tool": "mcp__fs__read_text_file", "args": {"path": str(mcp_root / "does-not-exist.txt")}}),
        json.dumps({"tool": "done", "result": "the file was missing, gave up"}),
    ]
    vfs = VFS(str(tmp_path / "vfs-root"))

    result = run_agentic_loop("read a missing file over MCP", vfs, _client(scripted_daemon), mcp_servers=[mcp_server])

    assert result.result == "the file was missing, gave up"
    assert result.turns_used == 2


def test_mcp_servers_from_env_is_empty_when_unset(monkeypatch):
    monkeypatch.delenv("AMH_MCP_SERVERS", raising=False)
    assert mcp_servers_from_env() == []


def test_mcp_servers_from_env_parses_real_config(monkeypatch):
    monkeypatch.setenv(
        "AMH_MCP_SERVERS",
        json.dumps([{"name": "fs", "command": "node", "args": ["server.js", "/tmp"], "env": {"FOO": "bar"}}]),
    )
    specs = mcp_servers_from_env()
    assert specs == [MCPServerSpec(name="fs", command="node", args=["server.js", "/tmp"], env={"FOO": "bar"})]
