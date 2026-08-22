"""Real agentic tool-calling loop for a subagent, closing the gap
workflows/goal.py's do_subagent_work has documented since it was
written: "a single-turn model completion... not an agentic loop with
VFS/tool access." Wires together the harness's VFS, planning
(write_todos), context/budget+compactor, and MCP client — all of which
existed with zero real consumers until now — into a real ReAct-style
loop: the model is given a JSON tool-call protocol (not native
function-calling, since the daemon's /v1/inference/complete route is a
plain-text completion, not an OpenAI/Anthropic tool-use wire shape — the
same reason Graphiti's own bridge uses a prompt-embedded schema rather
than native tool calls, see memory/graph_llm.py), and loops calling tools
against its own isolated VFS root and any configured MCP servers until it
emits a "done" action or exhausts max_turns.

MCP tool integration bridges harness/mcp_client.py's async session into
this otherwise-synchronous loop (it runs inside a synchronous DBOS step)
by making the whole loop body async internally and wrapping it in one
asyncio.run() at the public run_agentic_loop entry point — model_client.
complete() (a blocking HTTP call) runs via asyncio.to_thread so it
doesn't block MCP's own event loop. MCP servers are optional and
configured via AMH_MCP_SERVERS (see mcp_servers_from_env) — unset means
no MCP tools, the same optional-if-unconfigured pattern
workflows/memory_hooks.py already uses for Hindsight/Graphiti.

Every MCP tool call is proposed as an external effect through
workflows/operations.py (daemon/operations — §4) before it runs, using
model_client's own daemon_api_base_url/agent_token (no new plumbing
through run_agentic_loop's signature). This is deliberately track-only,
not enforcing: see _propose_mcp_effect's doc comment for why the loop
never waits for or acts on the resulting decision, and workflows/
operations.py's module doc comment for why these calls aren't
@DBOS.step()-wrapped here.

Physical actuation (workflows/actuate.py) is deliberately still not a
loop tool — that needs a policy decision about which tools an isolated
subagent should be allowed to reach directly versus only through an
explicit capability grant, not just an async bridge (which this module
now has and could reuse).
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import uuid
from contextlib import AsyncExitStack
from dataclasses import dataclass

from context.budget import BudgetManager, approximate_token_count
from context.compactor import Compactor
from context.llm import ModelClient
from workflows import operations

from .mcp_client import connect_stdio
from .planning import TodoList
from .vfs import VFS, PathEscapesRootError

logger = logging.getLogger(__name__)

_SYSTEM_PROMPT_TEMPLATE = """You are an isolated agent completing one task, with access to a virtual \
filesystem scoped to this task and a planning tool. Respond on every turn \
with ONLY a single JSON object — no other text, no markdown fences.

Available actions:
{tool_catalog}
{{"tool": "done", "result": "your final answer"}}

Call "done" as soon as the task is genuinely complete — do not call it \
prematurely, and do not keep working past completion. Your task:

{objective}"""

_BUILTIN_TOOL_CATALOG = """{"tool": "read_file", "args": {"path": "...", "offset": 0, "limit": null}}
{"tool": "write_file", "args": {"path": "...", "content": "..."}}
{"tool": "edit_file", "args": {"path": "...", "old": "...", "new": "..."}}
{"tool": "ls", "args": {"path": "."}}
{"tool": "glob", "args": {"pattern": "..."}}
{"tool": "grep", "args": {"pattern": "...", "path": "."}}
{"tool": "write_todos", "args": {"items": ["...", "..."]}}"""

_BUILTIN_TOOL_NAMES = {"read_file", "write_file", "edit_file", "ls", "glob", "grep", "write_todos"}

_BOOTSTRAP_MESSAGE = {"role": "user", "content": "Begin."}


@dataclass
class MCPServerSpec:
    name: str
    command: str
    args: list[str] | None = None
    env: dict[str, str] | None = None


def mcp_servers_from_env() -> list[MCPServerSpec]:
    """AMH_MCP_SERVERS, if set, is a JSON array of
    [{"name": "...", "command": "...", "args": [...], "env": {...}}, ...].
    Unset or empty means no MCP tools are available for this loop — the
    same optional-if-unconfigured pattern workflows/memory_hooks.py
    already uses for Hindsight/Graphiti; a malformed value, once someone
    HAS set it, is a real configuration error and raises rather than
    silently falling back to "no servers"."""
    raw = os.environ.get("AMH_MCP_SERVERS", "").strip()
    if not raw:
        return []
    specs = json.loads(raw)
    return [MCPServerSpec(name=s["name"], command=s["command"], args=s.get("args"), env=s.get("env")) for s in specs]


def _mcp_tool_name(server_name: str, tool_name: str) -> str:
    return f"mcp__{server_name}__{tool_name}"


@dataclass
class LoopResult:
    result: str
    turns_used: int
    compacted: bool


class LoopBudgetExceededError(Exception):
    """Raised when max_turns is exhausted without the model emitting a
    "done" action — a real failure the caller must handle, never
    silently returned as a best-effort partial answer."""


class UnknownToolError(Exception):
    """The model named a tool outside the known built-in + configured-MCP
    set — a real protocol violation, not swallowed as a no-op."""


async def _propose_mcp_effect(daemon_api_base_url: str, agent_token: str, server_name: str, raw_tool_name: str, args: dict) -> None:
    """Best-effort external-effect tracking (§4) for one MCP tool call, via
    workflows/operations.py. Always reversibility="none": an arbitrary
    third-party MCP tool has no verified inverse this harness can attest
    to, so daemon/policy's built-in policy always resolves this to
    needs_approval — and nothing here calls approve()/deny() on it. That's
    deliberate, not a bug: the effect record is a real, permanent audit
    entry ("this call happened, unattested"), not a live admission gate —
    the loop does not wait for or act on the decision, and still runs the
    tool call regardless of it. Enforcing admission before dispatch is a
    real, separate step this deliberately does not take yet.

    A failure to even record the Propose call (daemon unreachable, etc.)
    is logged and swallowed here, never allowed to block or crash the
    tool call it describes — the tool call's own success or failure is
    independent of whether tracking it succeeded."""
    try:
        await asyncio.to_thread(
            operations.propose,
            daemon_api_base_url,
            agent_token,
            str(uuid.uuid4()),
            "amh.core/mcp-client",
            f"mcp_tool_call:{server_name}:{raw_tool_name}",
            {"server": server_name, "tool": raw_tool_name, "args": args},
            "none",
        )
    except Exception:
        logger.warning(
            "failed to record operations effect for MCP call %s:%s (proceeding untracked)",
            server_name, raw_tool_name, exc_info=True,
        )


def _dispatch_builtin_tool(vfs: VFS, todos: TodoList, tool: str, args: dict) -> str:
    if tool == "read_file":
        return vfs.read_file(args["path"], args.get("offset", 0), args.get("limit"))
    if tool == "write_file":
        vfs.write_file(args["path"], args["content"])
        return f"wrote {args['path']}"
    if tool == "edit_file":
        vfs.edit_file(args["path"], args["old"], args["new"])
        return f"edited {args['path']}"
    if tool == "ls":
        entries = vfs.ls(args.get("path", "."))
        return "\n".join(f"{'d' if e.is_dir else 'f'} {e.path} ({e.size}b)" for e in entries) or "(empty)"
    if tool == "glob":
        return "\n".join(vfs.glob(args["pattern"])) or "(no matches)"
    if tool == "grep":
        hits = vfs.grep(args["pattern"], args.get("path", "."))
        return "\n".join(f"{p}:{n}:{line}" for p, n, line in hits) or "(no matches)"
    if tool == "write_todos":
        todos.write_todos(args["items"])
        return "todos updated"
    raise AssertionError(f"unreachable: {tool!r} is in _BUILTIN_TOOL_NAMES but has no dispatch branch")


async def _run_agentic_loop_async(
    objective: str,
    vfs: VFS,
    model_client: ModelClient,
    max_turns: int,
    budget: BudgetManager | None,
    compactor: Compactor | None,
    mcp_servers: list[MCPServerSpec],
) -> LoopResult:
    todos = TodoList(vfs)
    budget = budget or BudgetManager(count_tokens=approximate_token_count)
    compactor = compactor or Compactor()

    async with AsyncExitStack() as stack:
        mcp_clients = {}
        mcp_tool_lookup: dict[str, tuple[str, str]] = {}  # namespaced name -> (server_name, raw_tool_name)
        catalog_lines = [_BUILTIN_TOOL_CATALOG]

        for spec in mcp_servers:
            client = await stack.enter_async_context(connect_stdio(spec.command, spec.args, spec.env))
            mcp_clients[spec.name] = client
            for tool_info in await client.list_tools():
                namespaced = _mcp_tool_name(spec.name, tool_info.name)
                mcp_tool_lookup[namespaced] = (spec.name, tool_info.name)
                properties = tool_info.input_schema.get("properties", {}) if tool_info.input_schema else {}
                suffix = f" — {tool_info.description}" if tool_info.description else ""
                catalog_lines.append(f'{{"tool": "{namespaced}", "args": {json.dumps(properties)}}}{suffix}')

        all_tool_names = _BUILTIN_TOOL_NAMES | set(mcp_tool_lookup) | {"done"}
        system = _SYSTEM_PROMPT_TEMPLATE.format(objective=objective, tool_catalog="\n".join(catalog_lines))

        compacted_any = False
        for turn_index in range(max_turns):
            messages = [{"role": t.role, "content": t.content} for t in budget.turns] or [_BOOTSTRAP_MESSAGE]
            response_text = await asyncio.to_thread(model_client.complete, system, messages)
            budget.add_turn("assistant", response_text)

            try:
                action = json.loads(response_text)
            except json.JSONDecodeError as e:
                raise ValueError(f"agentic loop: model response was not valid JSON: {response_text!r}") from e
            if not isinstance(action, dict) or "tool" not in action:
                raise ValueError(f"agentic loop: model response missing 'tool': {action!r}")
            tool = action["tool"]
            if tool not in all_tool_names:
                raise UnknownToolError(f"unknown tool {tool!r} — must be one of {sorted(all_tool_names)}")

            if tool == "done":
                return LoopResult(result=str(action.get("result", "")), turns_used=turn_index + 1, compacted=compacted_any)

            args = action.get("args", {})
            if tool in mcp_tool_lookup:
                server_name, raw_tool_name = mcp_tool_lookup[tool]
                await _propose_mcp_effect(model_client.daemon_api_base_url, model_client.agent_token, server_name, raw_tool_name, args)
                try:
                    call_result = await mcp_clients[server_name].call_tool(raw_tool_name, args)
                    texts = [c["text"] for c in call_result.content if c.get("type") == "text"]
                    tool_result = "\n".join(texts) or "(no text content)"
                    if call_result.is_error:
                        tool_result = f"error: {tool_result}"
                except Exception as e:  # noqa: BLE001 — an MCP server's own failure is a recoverable tool error, not a loop crash
                    tool_result = f"error: {e}"
            else:
                try:
                    tool_result = _dispatch_builtin_tool(vfs, todos, tool, args)
                except (PathEscapesRootError, FileNotFoundError, ValueError, KeyError) as e:
                    tool_result = f"error: {e}"

            budget.add_turn("user", tool_result, is_tool_result=True)

            if compactor.compact(budget) is not None:
                compacted_any = True

        raise LoopBudgetExceededError(f"agentic loop for {objective!r} did not finish within {max_turns} turns")


def run_agentic_loop(
    objective: str,
    vfs: VFS,
    model_client: ModelClient,
    max_turns: int = 20,
    budget: BudgetManager | None = None,
    compactor: Compactor | None = None,
    mcp_servers: list[MCPServerSpec] | None = None,
) -> LoopResult:
    """Runs the loop to completion or raises. Never returns a fabricated
    or partial "success" — a malformed model response, an unknown tool
    name, or exhausting max_turns without a "done" action all propagate
    as real exceptions.

    Tool execution errors (a bad path, a missing file, an edit whose
    "old" text isn't found, an MCP server call that fails) are NOT
    propagated as loop failures — they are fed back to the model as a
    tool-result turn (prefixed "error:"), the same way a real coding
    agent sees its own mistakes and can correct them, rather than
    aborting the whole task over one bad tool call. An MCP server that
    fails to start at all (a bad command, a crash on startup) is a real
    configuration failure and does propagate — the operator explicitly
    configured that server, so a connection failure isn't the model's
    mistake to recover from.
    """
    return asyncio.run(
        _run_agentic_loop_async(objective, vfs, model_client, max_turns, budget, compactor, mcp_servers or [])
    )
