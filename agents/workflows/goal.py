"""Durable goal-pursuit workflow: pursue_goal / run_subagent, per
docs/AMH-SPECIFICATION.md Artifact F.

pursue_goal decomposes a goal into tasks (a step), spawns one run_subagent
child workflow per task (isolated context, condensed result-only return per
§14.2/Cognition), gathers results, and synthesizes (a single-threaded write,
per §3/§9). Both workflows are DBOS-durable: a crashed process resumes from
the last committed step on restart — no bespoke retry logic, no lost work.

decompose_goal and do_subagent_work make real model calls through the
daemon's inference seam (context/llm.py, daemon/inference) — they are not
canned responses, and this process never holds a model-provider credential
itself: daemon_api_base_url + agent_token (the same two values every other
daemon-calling step in this codebase already threads through — see
actuate.py) are all either step needs. Neither is wired to the
tool-calling harness (agents/harness/) yet: do_subagent_work is a
single-turn model completion answering the objective directly, not an
agentic loop with VFS/tool access. That harness integration is real
remaining work, stated plainly rather than papered over by a placeholder
that looked finished. Both steps propagate ModelNotConfiguredError
(context/llm.py) if no provider is registered on the daemon — they fail
loudly, not by returning a fake result.
"""

from __future__ import annotations

import json
from typing import Any

from dbos import DBOS

from context.llm import from_env
from context.observability import agent_run_span, inject_trace_context
from . import ontology

_DECOMPOSE_SYSTEM_PROMPT = """You decompose a goal into concrete, independently-workable tasks.

Respond with ONLY a JSON array of objects, each with one field "objective" \
(a short, self-contained instruction for one task). Produce as few tasks as \
the goal genuinely needs — a goal that is already one task should decompose \
to a single-element array. Do not include any text outside the JSON array."""


@DBOS.step()
def decompose_goal(goal_id: str, goal_text: str, db_path: str, daemon_api_base_url: str, agent_token: str) -> list[dict[str, str]]:
    """Decomposes goal_text into tasks via a real model call through the
    daemon's inference seam, parses the model's JSON response, and
    persists each task. Raises context.llm.ModelNotConfiguredError if no
    model provider is registered on the daemon, or ValueError if the
    model's response is not the requested JSON shape — never silently
    falls back to a fake decomposition."""
    ontology.ensure_goal(db_path, goal_id, goal_text)

    client = from_env(daemon_api_base_url, agent_token)
    response_text = client.complete(
        system=_DECOMPOSE_SYSTEM_PROMPT,
        messages=[{"role": "user", "content": goal_text}],
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


_SUBAGENT_SYSTEM_PROMPT = """You are an isolated sub-agent completing one task on behalf of a manager agent. \
Address the objective directly and return your result. Your response is the \
only thing the manager will see — it does not see how you arrived at it."""


@DBOS.step()
def do_subagent_work(task_id: str, objective: str, db_path: str, run_id: str, daemon_api_base_url: str, agent_token: str) -> dict[str, Any]:
    """Real model call answering objective directly, through the daemon's
    inference seam. Returns a condensed result only (per Artifact D's
    context_ref/trace_ref design: the manager never sees the child's full
    transcript unless it asks). This is a single-turn completion, not yet
    an agentic tool-calling loop — see this module's top-level doc
    comment."""
    ontology.log_event(db_path, run_id, "subagent.work", {"task_id": task_id, "objective": objective})
    client = from_env(daemon_api_base_url, agent_token)
    result_text = client.complete(
        system=_SUBAGENT_SYSTEM_PROMPT,
        messages=[{"role": "user", "content": objective}],
    )
    return {"task_id": task_id, "status": "done", "summary": result_text}


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
    """Starts run_subagent as a DBOS child workflow, capturing the
    caller's current OTel span context and passing it through explicitly
    so the child's span nests under the caller's trace. Must be called
    from inside the caller's own `with agent_run_span(...):` block — the
    whole point is to capture whatever span is active at the call site.

    The single choke point for starting a sub-agent: both pursue_goal and
    run_greenhouse_scenario call this rather than DBOS.start_workflow
    directly, so trace propagation only needs to be right in one place.
    """
    trace_context = inject_trace_context()
    return DBOS.start_workflow(run_subagent, task_id, objective, db_path, daemon_api_base_url, agent_token, trace_context)


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
    run_subagent children are not re-run."""
    with agent_run_span(agent_id=goal_id):
        tasks = decompose_goal(goal_id, goal_text, db_path, daemon_api_base_url, agent_token)

        handles = [start_subagent(t["task_id"], t["objective"], db_path, daemon_api_base_url, agent_token) for t in tasks]
        gathered = [h.get_result() for h in handles]

        return synthesize(goal_id, gathered, db_path)
