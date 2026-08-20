"""The `task`-tool subagent contract: state-stripping isolation, result-only
return, per docs/AMH-SPECIFICATION.md §14.2. A spawned subagent gets its own
VFS root — a fresh, isolated filesystem the parent cannot see into and the
child cannot escape — and returns a condensed result plus a trace_ref
handle the parent can pull during synthesis (Cognition's "share full
traces, not just messages", surfaced only on request, never injected
wholesale into peer contexts).

This module owns context isolation (the VFS boundary + result shape).
Actual execution is workflows.goal.run_subagent (the DBOS-durable child
workflow) — the harness and the durability layer are deliberately
decoupled: either can be swapped without touching the other.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from .vfs import VFS

# Only these keys ever cross the isolation boundary back to the parent —
# anything else the child produced (its full VFS, its internal reasoning)
# stays in its own root unless the parent explicitly follows trace_ref.
CONDENSED_RESULT_KEYS = {"task_id", "status", "summary"}


@dataclass
class SubagentHandle:
    task_id: str
    vfs: VFS
    trace_ref: str  # vfs:// handle the parent may pull during synthesis


def spawn(workspace_root: str, task_id: str) -> SubagentHandle:
    """Allocates an isolated VFS root for one subagent run.

    workspace_root is the overall *run's* workspace — not the parent
    agent's own VFS root. Subagent roots are siblings under
    workspace_root/subagents/<task_id>, deliberately outside any parent
    VFS's root directory: a parent VFS confines glob/ls/grep to its own
    root (see VFS._resolve), so if a child's files lived inside the
    parent's root the parent could walk straight into them despite the
    "isolated" framing. Keeping them as siblings makes that boundary real
    rather than nominal — the parent can only reach a child's files via
    the explicit trace_ref handle, never by listing its own root.
    """
    child_root = str(Path(workspace_root) / "subagents" / task_id)
    return SubagentHandle(
        task_id=task_id,
        vfs=VFS(child_root),
        trace_ref=f"vfs://{child_root}/trace.jsonl",
    )


def condense(raw_result: dict) -> dict:
    """Strips a subagent's raw result down to the result-only contract —
    called on whatever run_subagent (or any future execution engine)
    returns, before it's handed back to the parent workflow."""
    return {k: v for k, v in raw_result.items() if k in CONDENSED_RESULT_KEYS}
