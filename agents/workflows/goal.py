"""Durable goal-pursuit workflow: pursue_goal / run_subagent, per
docs/AMH-SPECIFICATION.md Artifact F.

pursue_goal decomposes a goal into tasks (a step), spawns one run_subagent
child workflow per task (isolated context, condensed result-only return per
§14.2/Cognition), gathers results, and synthesizes (a single-threaded write,
per §3/§9). Both workflows are DBOS-durable: a crashed process resumes from
the last committed step on restart — no bespoke retry logic, no lost work.

V0 scope: decomposition and subagent execution are intentionally simple
placeholders (task 8, the deep-agent harness, is where planning/VFS/tool-use
gets real). What this module proves is the durability contract itself:
pursue_goal survives a process restart mid-flight.
"""

from __future__ import annotations

from typing import Any

from dbos import DBOS

from context.observability import agent_run_span, inject_trace_context
from . import ontology


@DBOS.step()
def decompose_goal(goal_id: str, goal_text: str, db_path: str) -> list[dict[str, str]]:
    """V0 placeholder decomposition: one task per ';'-separated clause in
    goal_text, or the whole goal as a single task if there's no ';'.
    Real planning (write_todos, multi-step decomposition) is task 8's harness.
    """
    ontology.ensure_goal(db_path, goal_id, goal_text)
    clauses = [c.strip() for c in goal_text.split(";") if c.strip()] or [goal_text]
    tasks = []
    for clause in clauses:
        task_id = ontology.create_task(db_path, goal_id, clause)
        tasks.append({"task_id": task_id, "objective": clause})
    return tasks


@DBOS.step()
def do_subagent_work(task_id: str, objective: str, db_path: str, run_id: str) -> dict[str, Any]:
    """V0 placeholder for the isolated sub-agent's actual work. Returns a
    condensed result only (per Artifact D's context_ref/trace_ref design:
    the manager never sees the child's full transcript unless it asks).
    """
    ontology.log_event(db_path, run_id, "subagent.work", {"task_id": task_id, "objective": objective})
    return {"task_id": task_id, "status": "done", "summary": f"completed: {objective}"}


@DBOS.workflow()
def run_subagent(task_id: str, objective: str, db_path: str, trace_context: dict[str, str] | None = None) -> dict[str, Any]:
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
            result = do_subagent_work(task_id, objective, db_path, run_id)
            ontology.set_task_status(db_path, task_id, "done")
            ontology.end_run(db_path, run_id, "ok")
            return result
        except Exception:
            ontology.set_task_status(db_path, task_id, "failed")
            ontology.end_run(db_path, run_id, "error")
            raise


def start_subagent(task_id: str, objective: str, db_path: str):
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
    return DBOS.start_workflow(run_subagent, task_id, objective, db_path, trace_context)


@DBOS.step()
def synthesize(goal_id: str, gathered: list[dict[str, Any]], db_path: str) -> str:
    """Single-threaded write, per Cognition's map-reduce-and-manage
    guidance (§3): many subagents may run concurrently, but only one
    synthesis step ever writes the final result."""
    summary = "; ".join(r.get("summary", "") for r in gathered)
    ontology.set_goal_status(db_path, goal_id, "done")
    return summary


@DBOS.workflow()
def pursue_goal(goal_id: str, goal_text: str, db_path: str) -> str:
    """Top-level durable workflow. Decomposes, fans out to run_subagent
    child workflows, gathers, synthesizes. If the process dies mid-flight,
    restarting it (with the same DBOS system database) resumes exactly
    where it left off — decompose_goal and any already-completed
    run_subagent children are not re-run."""
    with agent_run_span(agent_id=goal_id):
        tasks = decompose_goal(goal_id, goal_text, db_path)

        handles = [start_subagent(t["task_id"], t["objective"], db_path) for t in tasks]
        gathered = [h.get_result() for h in handles]

        return synthesize(goal_id, gathered, db_path)
