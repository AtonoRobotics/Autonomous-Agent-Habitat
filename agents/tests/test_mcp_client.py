"""Exercises harness.mcp_client against a real third-party MCP server: the
official @modelcontextprotocol/server-filesystem reference server. This is
a genuine protocol round-trip (stdio transport, JSON-RPC
initialize/list_tools/call_tool) against server code AMH does not own —
not a mock of the MCP wire protocol.

The server is pinned as a local npm dependency under
fixtures/mcp-servers/ (run `npm install` there once) and invoked directly
via `node .../dist/index.js` rather than through `npx`, which re-checks
the npm registry on every invocation (~70s) even when the package is
already cached — invoking the installed binary directly is what actually
makes this fast and deterministic in CI.

Skipped if the fixture hasn't been installed or node is unavailable.
"""

from __future__ import annotations

import os
import shutil

import pytest

from harness.mcp_client import connect_stdio

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SERVER_ENTRYPOINT = os.path.join(
    REPO_ROOT, "fixtures", "mcp-servers", "node_modules",
    "@modelcontextprotocol", "server-filesystem", "dist", "index.js",
)

pytestmark = pytest.mark.skipif(
    shutil.which("node") is None or not os.path.exists(SERVER_ENTRYPOINT),
    reason="node unavailable or fixtures/mcp-servers/ not installed (run `npm install` there)",
)


@pytest.mark.asyncio
async def test_list_tools_from_real_filesystem_server(tmp_path):
    async with connect_stdio("node", [SERVER_ENTRYPOINT, str(tmp_path)]) as client:
        tools = await client.list_tools()
        tool_names = {t.name for t in tools}
        assert "read_text_file" in tool_names
        assert "write_file" in tool_names


@pytest.mark.asyncio
async def test_call_tool_writes_and_reads_a_real_file(tmp_path):
    async with connect_stdio("node", [SERVER_ENTRYPOINT, str(tmp_path)]) as client:
        write_result = await client.call_tool(
            "write_file",
            {"path": str(tmp_path / "hello.txt"), "content": "hello from MCP"},
        )
        assert write_result.is_error is False

        # The file must actually exist on disk — proving the tool call
        # reached the real server process and had a real effect, not a
        # stub.
        assert (tmp_path / "hello.txt").read_text() == "hello from MCP"

        read_result = await client.call_tool("read_text_file", {"path": str(tmp_path / "hello.txt")})
        assert read_result.is_error is False
        text_blocks = [c["text"] for c in read_result.content if c.get("type") == "text"]
        assert any("hello from MCP" in t for t in text_blocks)
