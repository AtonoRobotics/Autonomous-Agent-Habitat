"""MCP client (stdio transport), per docs/AMH-SPECIFICATION.md §12.

AMH is both an MCP client (consuming third-party servers) and an MCP
server (exposing its own capabilities) — this module is the client half,
implementing stdio transport. Streamable HTTP is a separate transport this
module does not implement; a caller needing a remote MCP server over HTTP
needs that added here first.

Security posture (§12, non-negotiable): tool output from an MCP server is
untrusted input, not agent-authored content. This client does not execute
or interpret tool results — it hands them back as data for the caller
(ultimately the harness's context/budget layer) to route through the same
tool_result_cap and prompt-injection-aware handling as any other tool
output. Nothing here allowlists tools automatically; that's a harness-level
policy decision, not a transport concern.
"""

from __future__ import annotations

from contextlib import asynccontextmanager
from dataclasses import dataclass
from typing import Any, AsyncIterator

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client


@dataclass
class ToolInfo:
    name: str
    description: str | None
    input_schema: dict[str, Any]


@dataclass
class ToolCallResult:
    content: list[dict[str, Any]]
    is_error: bool


@asynccontextmanager
async def connect_stdio(command: str, args: list[str] | None = None, env: dict[str, str] | None = None) -> AsyncIterator["MCPClient"]:
    """Launches a local MCP server subprocess over stdio and yields a
    connected, initialized MCPClient. The subprocess is terminated when
    the context manager exits — no orphaned server processes."""
    params = StdioServerParameters(command=command, args=args or [], env=env)
    async with stdio_client(params) as (read, write):
        async with ClientSession(read, write) as session:
            await session.initialize()
            yield MCPClient(session)


class MCPClient:
    def __init__(self, session: ClientSession):
        self._session = session

    async def list_tools(self) -> list[ToolInfo]:
        result = await self._session.list_tools()
        return [
            ToolInfo(name=t.name, description=t.description, input_schema=t.input_schema)
            for t in result.tools
        ]

    async def call_tool(self, name: str, arguments: dict[str, Any]) -> ToolCallResult:
        result = await self._session.call_tool(name, arguments)
        content = [c.model_dump() for c in result.content]
        return ToolCallResult(content=content, is_error=bool(result.is_error))
