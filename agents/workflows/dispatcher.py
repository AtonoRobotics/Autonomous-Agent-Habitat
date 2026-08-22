"""Goal dispatcher: the missing piece connecting a durably-created `goal`
row (however it was created — daemon/a2a's SendMessage today, any future
path tomorrow) to actually running pursue_goal for it.

docs/AMH-SPECIFICATION.md §3.1 requires "deterministic delivery of
external triggers into DBOS using stable idempotency keys" as an
amh-daemon responsibility. Before this module, nothing in this codebase
provided that: pursue_goal is real and durable, but every existing call
site invokes it directly — a goal created via A2A just sat in
TASK_STATE_SUBMITTED forever, because nothing was watching for it. This
module is that watcher.

claim_and_dispatch_once is the whole dispatch mechanism: for every
still-'open' goal (workflows.ontology.list_open_goals — a plain read),
it starts pursue_goal under a workflow ID derived deterministically from
the goal's own id (f"goal-{goal_id}"). DBOS's own workflow-id
deduplication is what satisfies "stable idempotency keys" —
DBOS.start_workflow under a workflow ID it has already seen returns a
handle to the existing run rather than starting a second one, so calling
this repeatedly for the same goal_id (even concurrently, from two
different dispatcher processes) never double-dispatches it. Deliberately
NOT a mutating "claim" step (flip status, then start the workflow): that
two-step sequence has a crash window in between where a goal could end
up claimed but never actually dispatched. Relying purely on DBOS's own
dedup means there's no such window — the cost is that a goal already in
flight (not yet 'done') gets a harmless, idempotent re-dispatch attempt
on every poll tick until it completes, since nothing here marks it
non-'open' early. That's deliberate, not an oversight: see this module's
scope note in README for why goal.py's own status transitions are left
untouched rather than adding one here to suppress the redundant polling.

run_forever is a standing process — the first genuinely long-running
Python process this codebase has: init_dbos + DBOS.launch() once, then
claim_and_dispatch_once on an interval until interrupted. How to actually
deploy and supervise that process (systemd unit, process manager,
however many replicas) is deliberately out of scope here, the same
"mechanism, not deployment tooling" line daemon/store.Rollback's CLI flag
already draws for schema rollback.
"""

from __future__ import annotations

import logging
import os
import time

from dbos import DBOS, SetWorkflowID

from . import ontology
from .goal import pursue_goal
from .runtime import init_dbos

logger = logging.getLogger(__name__)


def claim_and_dispatch_once(db_path: str, daemon_api_base_url: str, agent_token: str) -> list[str]:
    """Ensures pursue_goal has been started for every still-'open' goal.
    Returns those goals' ids. Requires DBOS to already be launched (see
    run_forever) — this function only starts workflows, it doesn't manage
    the DBOS lifecycle itself, so tests can call it against an
    already-running DBOS instance without also owning launch/destroy."""
    open_goals = ontology.list_open_goals(db_path)
    goal_ids = []
    for goal_id, goal_text in open_goals:
        workflow_id = f"goal-{goal_id}"
        with SetWorkflowID(workflow_id):
            DBOS.start_workflow(pursue_goal, goal_id, goal_text, db_path, daemon_api_base_url, agent_token)
        goal_ids.append(goal_id)
    return goal_ids


def run_forever(db_path: str, daemon_api_base_url: str, agent_token: str, poll_interval_sec: float = 2.0) -> None:
    """Standing dispatcher process: launches DBOS once, then claims and
    dispatches newly-open goals on an interval until interrupted
    (KeyboardInterrupt/SIGTERM propagates as a normal exception here —
    DBOS.destroy() in the finally block is what makes a restart clean)."""
    init_dbos("amh-goal-dispatcher", db_path)
    DBOS.launch()
    logger.info("goal dispatcher started, polling every %ss", poll_interval_sec)
    try:
        while True:
            dispatched = claim_and_dispatch_once(db_path, daemon_api_base_url, agent_token)
            if dispatched:
                logger.info("dispatched %d goal(s): %s", len(dispatched), dispatched)
            time.sleep(poll_interval_sec)
    finally:
        DBOS.destroy()


if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO)
    run_forever(
        db_path=os.environ["DATABASE_URL"],
        daemon_api_base_url=os.environ["AMH_API_BASE_URL"],
        agent_token=os.environ["AMH_API_AGENT_TOKEN"],
        poll_interval_sec=float(os.environ.get("AMH_DISPATCHER_POLL_INTERVAL_SEC", "2.0")),
    )
