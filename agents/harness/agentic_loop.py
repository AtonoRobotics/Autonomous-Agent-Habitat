"""Real agentic tool-calling loop for a subagent, closing the gap
workflows/goal.py's do_subagent_work has documented since it was
written: "a single-turn model completion... not an agentic loop with
VFS/tool access." Wires together the harness's VFS, planning
(write_todos), and context/budget+compactor — all of which existed with
zero real consumers until now — into a real ReAct-style loop: the model
is given a JSON tool-call protocol (not native function-calling, since
the daemon's /v1/inference/complete route is a plain-text completion, not
an OpenAI/Anthropic tool-use wire shape — the same reason Graphiti's own
bridge uses a prompt-embedded schema rather than native tool calls, see
memory/graph_llm.py), and loops calling tools against its own isolated
VFS root until it emits a "done" action or exhausts max_turns.

The tool surface is deliberately VFS + planning only for now — MCP tool
integration (harness/mcp_client.py) and physical actuation
(workflows/actuate.py) are real, separate follow-up work, not silently
dropped: both need an async<->sync bridge (mcp_client.py's session is
async; this loop is sync, to run inside a DBOS step) and a policy
decision about which tools an isolated subagent should be allowed to
reach directly versus only through an explicit capability grant.
"""

from __future__ import annotations

import json
from dataclasses import dataclass

from context.budget import BudgetManager, approximate_token_count
from context.compactor import Compactor
from context.llm import ModelClient

from .planning import TodoList
from .vfs import VFS, PathEscapesRootError

_SYSTEM_PROMPT_TEMPLATE = """You are an isolated agent completing one task, with access to a virtual \
filesystem scoped to this task and a planning tool. Respond on every turn \
with ONLY a single JSON object — no other text, no markdown fences.

Available actions:
{{"tool": "read_file", "args": {{"path": "...", "offset": 0, "limit": null}}}}
{{"tool": "write_file", "args": {{"path": "...", "content": "..."}}}}
{{"tool": "edit_file", "args": {{"path": "...", "old": "...", "new": "..."}}}}
{{"tool": "ls", "args": {{"path": "."}}}}
{{"tool": "glob", "args": {{"pattern": "..."}}}}
{{"tool": "grep", "args": {{"pattern": "...", "path": "."}}}}
{{"tool": "write_todos", "args": {{"items": ["...", "..."]}}}}
{{"tool": "done", "result": "your final answer"}}

Call "done" as soon as the task is genuinely complete — do not call it \
prematurely, and do not keep working past completion. Your task:

{objective}"""

_BOOTSTRAP_MESSAGE = {"role": "user", "content": "Begin."}

_TOOL_NAMES = {"read_file", "write_file", "edit_file", "ls", "glob", "grep", "write_todos", "done"}


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
    """The model named a tool outside _TOOL_NAMES — a real protocol
    violation, not swallowed as a no-op."""


def _dispatch_tool(vfs: VFS, todos: TodoList, tool: str, args: dict) -> str:
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
    raise AssertionError(f"unreachable: {tool!r} is in _TOOL_NAMES but has no dispatch branch")


def run_agentic_loop(
    objective: str,
    vfs: VFS,
    model_client: ModelClient,
    max_turns: int = 20,
    budget: BudgetManager | None = None,
    compactor: Compactor | None = None,
) -> LoopResult:
    """Runs the loop to completion or raises. Never returns a fabricated
    or partial "success" — a malformed model response, an unknown tool
    name, or exhausting max_turns without a "done" action all propagate
    as real exceptions.

    Tool execution errors (a bad path, a missing file, an edit whose
    "old" text isn't found) are NOT propagated as loop failures — they
    are fed back to the model as a tool-result turn (prefixed "error:"),
    the same way a real coding agent sees its own mistakes and can
    correct them, rather than aborting the whole task over one bad
    tool call.
    """
    todos = TodoList(vfs)
    budget = budget or BudgetManager(count_tokens=approximate_token_count)
    compactor = compactor or Compactor()
    system = _SYSTEM_PROMPT_TEMPLATE.format(objective=objective)

    compacted_any = False
    for turn_index in range(max_turns):
        messages = [{"role": t.role, "content": t.content} for t in budget.turns] or [_BOOTSTRAP_MESSAGE]
        response_text = model_client.complete(system=system, messages=messages)
        budget.add_turn("assistant", response_text)

        try:
            action = json.loads(response_text)
        except json.JSONDecodeError as e:
            raise ValueError(f"agentic loop: model response was not valid JSON: {response_text!r}") from e
        if not isinstance(action, dict) or "tool" not in action:
            raise ValueError(f"agentic loop: model response missing 'tool': {action!r}")
        if action["tool"] not in _TOOL_NAMES:
            raise UnknownToolError(f"unknown tool {action['tool']!r} — must be one of {sorted(_TOOL_NAMES)}")

        if action["tool"] == "done":
            return LoopResult(result=str(action.get("result", "")), turns_used=turn_index + 1, compacted=compacted_any)

        try:
            tool_result = _dispatch_tool(vfs, todos, action["tool"], action.get("args", {}))
        except (PathEscapesRootError, FileNotFoundError, ValueError, KeyError) as e:
            tool_result = f"error: {e}"

        budget.add_turn("user", tool_result, is_tool_result=True)

        if compactor.compact(budget) is not None:
            compacted_any = True

    raise LoopBudgetExceededError(f"agentic loop for {objective!r} did not finish within {max_turns} turns")
