"""Durable goal-pursuit workflow: pursue_goal / run_subagent, per
docs/AMH-SPECIFICATION.md Artifact F.

pursue_goal decomposes a goal into tasks (a step), spawns one run_subagent
child workflow per task (isolated context, condensed result-only return per
§14.2/Cognition) subject to a durable, bounded concurrency limit
(_SUBAGENT_QUEUE — §14 V1's "isolated subordinate-agent workflows with
bounded concurrency"), gathers results, and synthesizes (a single-threaded
write, per §3/§9). Both workflows are DBOS-durable: a crashed process
resumes from the last committed step on restart — no bespoke retry logic,
no lost work, and a subagent still queued (not yet started) at crash time
is still subject to the same concurrency bound on restart.

decompose_goal and do_subagent_work make real model calls through the
daemon's inference seam (context/llm.py, daemon/inference) — they are not
canned responses, and this process never holds a model-provider credential
itself: daemon_api_base_url + agent_token (the same two values every other
daemon-calling step in this codebase already threads through — see
actuate.py) are all either step needs. do_subagent_work runs a real
agentic tool-calling loop (harness/agentic_loop.py) against an isolated
VFS root, not a single-turn completion — see that module's docstring for
the tool-call protocol and why it isn't native function-calling. Both
steps propagate ModelNotConfiguredError (context/llm.py) if no provider
is registered on the daemon — they fail loudly, not by returning a fake
result.
"""

from __future__ import annotations

import json
import os
import tempfile
from typing import Any

from dbos import DBOS, Queue

from context.llm import from_env
from context.observability import agent_run_span, inject_trace_context
from harness.vfs import VFS
from harness.agentic_loop import mcp_servers_from_env, run_agentic_loop
from memory.working import project_working_memory
from . import ontology
from .memory_hooks import recall_context, retain_outcome

# Bounded concurrency for subordinate-agent fan-out (docs/AMH-SPECIFICATION.md
# §14, V1: "isolated subordinate-agent workflows with bounded concurrency").
# A goal that decomposes into many tasks must not fan out unboundedly —
# every task at once would mean unbounded concurrent model calls (cost,
# rate limits) and unbounded concurrent SQLite writers against the shared
# ontology database. DBOS's Queue is a durable concurrency limiter, not an
# in-process one (e.g. threading.Semaphore): a queued-but-not-yet-started
# subagent survives a process crash and resumes being subject to the same
# limit on restart, matching every other durability guarantee in this
# codebase. AMH_SUBAGENT_CONCURRENCY is read once at import time, since a
# DBOS Queue's concurrency is fixed at construction.
_SUBAGENT_QUEUE = Queue(
    "amh-subagents",
    concurrency=int(os.environ.get("AMH_SUBAGENT_CONCURRENCY", "5")),
    # DBOS's default (1.0s) is tuned for low background-poll overhead at
    # scale; a habitat spawning subagents on human/device timescales (not
    # thousands/sec) can afford to check far more often, and callers of
    # start_subagent already block on .get_result() — a slow dequeue is
    # pure added latency with no offsetting benefit here.
    polling_interval_sec=0.2,
)

_DECOMPOSE_SYSTEM_PROMPT = """You decompose a goal into concrete, independently-workable tasks.

Respond with ONLY a JSON array of objects, each with one field "objective" \
(a short, self-contained instruction for one task). Produce as few tasks as \
the goal genuinely needs — a goal that is already one task should decompose \
to a single-element array. Do not include any text outside the JSON array."""


@DBOS.step()
def decompose_goal(
    goal_id: str, goal_text: str, db_path: str, daemon_api_base_url: str, agent_token: str, memory_context: str = ""
) -> list[dict[str, str]]:
    """Decomposes goal_text into tasks via a real model call through the
    daemon's inference seam, parses the model's JSON response, and
    persists each task. Raises context.llm.ModelNotConfiguredError if no
    model provider is registered on the daemon, or ValueError if the
    model's response is not the requested JSON shape — never silently
    falls back to a fake decomposition.

    memory_context, when non-empty, is recalled episodic/semantic memory
    (workflows/memory_hooks.recall_context) prepended to the user message
    — past goals and known facts relevant to this one, if any were found."""
    ontology.ensure_goal(db_path, goal_id, goal_text)

    user_content = f"{memory_context}\n\n{goal_text}" if memory_context else goal_text
    client = from_env(daemon_api_base_url, agent_token)
    response_text = client.complete(
        system=_DECOMPOSE_SYSTEM_PROMPT,
        messages=[{"role": "user", "content": user_content}],
    )
    try:
        parsed = json.loads(response_text)
    except json.JSONDecodeError as e:
        raise ValueError(f"decompose_goal: model response was not valid JSON: {response_text!r}") from e
    if not isinstance(parsed, list) or not parsed or not all(isinstance(t, dict) and "objective" in t for t in parsed):
        raise ValueError(f"decompose_goal: model response was not a non-empty JSON array of {{'objective': ...}}: {parsed!r}")

    tasks = []
    for item in parsed:
        objective = str(item["objective"]).strip()
        if not objective:
            continue
        task_id = ontology.create_task(db_path, goal_id, objective)
        tasks.append({"task_id": task_id, "objective": objective})
    if not tasks:
        raise ValueError(f"decompose_goal: model produced no usable tasks for goal {goal_id!r}")
    return tasks


def _workspace_root() -> str:
    """Root directory subagent VFS roots are nested under, one per run_id
    — AMH_WORKSPACE_ROOT for a real deployment's durable-enough scratch
    space; a per-process tmp dir otherwise. Not itself durable state (a
    subagent's files are its own working scratch, not part of the
    ontology) — see harness/vfs.py."""
    return os.environ.get("AMH_WORKSPACE_ROOT", os.path.join(tempfile.gettempdir(), "amh-workspaces"))


@DBOS.step()
def do_subagent_work(task_id: str, objective: str, db_path: str, run_id: str, daemon_api_base_url: str, agent_token: str) -> dict[str, Any]:
    """Runs a real agentic tool-calling loop (harness/agentic_loop.py)
    against an isolated VFS root scoped to this run — not a single-turn
    completion. Returns a condensed result only (per Artifact D's
    context_ref/trace_ref design: the manager never sees the child's full
    transcript unless it asks); the full turn-by-turn transcript lives in
    the run's own VFS root, reachable only via that explicit handle.

    Working memory (memory.working.project_working_memory) surfaces the
    parent goal's text as context — without it, a subagent sees only its
    own isolated objective and never learns what larger goal it serves."""
    ontology.log_event(db_path, run_id, "subagent.work", {"task_id": task_id, "objective": objective})

    working_memory = project_working_memory(db_path, run_id)
    if working_memory and working_memory.goal_text:
        full_objective = f"Overall goal: {working_memory.goal_text}\n\nYour task: {objective}"
    else:
        full_objective = objective

    client = from_env(daemon_api_base_url, agent_token)
    vfs = VFS(os.path.join(_workspace_root(), run_id))
    loop_result = run_agentic_loop(full_objective, vfs, client, mcp_servers=mcp_servers_from_env())
    return {"task_id": task_id, "status": "done", "summary": loop_result.result}


@DBOS.workflow()
def run_subagent(task_id: str, objective: str, db_path: str, daemon_api_base_url: str, agent_token: str, trace_context: dict[str, str] | None = None) -> dict[str, Any]:
    """Runs as an isolated DBOS child workflow — crash-recoverable
    independently of the parent (§14.2's subagent isolation contract).

    trace_context (see start_subagent below) restores this span as a
    child of the caller's trace, rather than starting an unrelated one —
    DBOS.start_workflow runs this on its own worker thread with no
    ambient OTel context otherwise."""
    with agent_run_span(agent_id=task_id, trace_context=trace_context):
        run_id = ontology.create_run(db_path, task_id)
        ontology.set_task_status(db_path, task_id, "active")
        try:
            result = do_subagent_work(task_id, objective, db_path, run_id, daemon_api_base_url, agent_token)
            ontology.set_task_status(db_path, task_id, "done")
            ontology.end_run(db_path, run_id, "ok")
            return result
        except Exception:
            ontology.set_task_status(db_path, task_id, "failed")
            ontology.end_run(db_path, run_id, "error")
            raise


def start_subagent(task_id: str, objective: str, db_path: str, daemon_api_base_url: str, agent_token: str):
    """Enqueues run_subagent as a DBOS child workflow, capturing the
    caller's current OTel span context and passing it through explicitly
    so the child's span nests under the caller's trace. Must be called
    from inside the caller's own `with agent_run_span(...):` block — the
    whole point is to capture whatever span is active at the call site.

    Goes through _SUBAGENT_QUEUE rather than DBOS.start_workflow directly:
    a goal that decomposes into more tasks than AMH_SUBAGENT_CONCURRENCY
    allows queues the excess durably instead of running them all at once
    — see this module's top-level comment. The returned handle behaves
    identically either way (.get_result() blocks until the subagent
    actually runs and completes), so callers don't need to know which.

    The single choke point for starting a sub-agent: both pursue_goal and
    run_greenhouse_scenario call this rather than the queue/start_workflow
    APIs directly, so trace propagation and the concurrency bound only
    need to be right in one place.
    """
    trace_context = inject_trace_context()
    return _SUBAGENT_QUEUE.enqueue(run_subagent, task_id, objective, db_path, daemon_api_base_url, agent_token, trace_context)


@DBOS.step()
def synthesize(goal_id: str, gathered: list[dict[str, Any]], db_path: str) -> str:
    """Single-threaded write, per Cognition's map-reduce-and-manage
    guidance (§3): many subagents may run concurrently, but only one
    synthesis step ever writes the final result."""
    summary = "; ".join(r.get("summary", "") for r in gathered)
    ontology.set_goal_status(db_path, goal_id, "done")
    return summary


@DBOS.workflow()
def pursue_goal(goal_id: str, goal_text: str, db_path: str, daemon_api_base_url: str, agent_token: str) -> str:
    """Top-level durable workflow. Decomposes, fans out to run_subagent
    child workflows, gathers, synthesizes. If the process dies mid-flight,
    restarting it (with the same DBOS system database) resumes exactly
    where it left off — decompose_goal and any already-completed
    run_subagent children are not re-run.

    Recalls episodic/semantic memory (workflows/memory_hooks.recall_context)
    before decomposing, and retains the outcome (retain_outcome) after
    synthesizing — best-effort, a no-op when Hindsight/Graphiti aren't
    configured (see memory_hooks's module docstring)."""
    with agent_run_span(agent_id=goal_id):
        memory_context = recall_context(goal_text, daemon_api_base_url, agent_token)
        tasks = decompose_goal(goal_id, goal_text, db_path, daemon_api_base_url, agent_token, memory_context)

        handles = [start_subagent(t["task_id"], t["objective"], db_path, daemon_api_base_url, agent_token) for t in tasks]
        gathered = [h.get_result() for h in handles]

        summary = synthesize(goal_id, gathered, db_path)
        retain_outcome(goal_text, summary, daemon_api_base_url, agent_token)
        return summary
