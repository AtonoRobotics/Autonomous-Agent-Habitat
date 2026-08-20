"""The V0 walking-skeleton scenario, per docs/AMH-SPECIFICATION.md
Artifact H: "Keep the greenhouse healthy overnight; if temperature exceeds
32C, open the vent." Composes steps 1-4 of the worked scenario:

  1. Decomposition (goal.decompose_goal)
  2. Sub-agent spawn with context isolation (goal.run_subagent)
  3. Context compaction under sustained polling (context.budget/compactor)
  4. Autonomous reversible device actuation, no ApprovalGate — the vent
     action has a verified inverse (actuate.actuate_device, bridging to
     the Go daemon's actuation kernel over SSH)

Step 3 is deliberately not itself a DBOS step: BudgetManager/Compactor are
in-process context-window bookkeeping, not durable state — there is
nothing to replay if the process restarts mid-compaction; the model simply
re-derives its working context on resume, which is the whole point of
compaction existing at the harness layer rather than the durability layer.
"""

from __future__ import annotations

from typing import Any

from dbos import DBOS

from context.budget import BudgetManager
from context.compactor import Compactor
from .actuate import actuate_device
from .goal import decompose_goal, run_subagent, synthesize


def simulate_overnight_polling(poll_count: int = 12) -> dict[str, Any]:
    """Step 3: simulates the sustained temperature-polling turns that
    eventually cross the compaction threshold, per Artifact H step 3.
    Returns whether compaction fired and how many turns it compacted."""
    budget = BudgetManager(window_budget=1_000, compact_at=0.70)
    for i in range(poll_count):
        budget.add_turn("tool", f"temperature reading #{i}: 28.{i}C, vent closed" * 10, is_tool_result=True)

    compactor = Compactor(keep_recent_turns_raw=3)
    result = compactor.compact(budget)
    return {
        "compacted": result is not None,
        "turns_compacted": result.turns_compacted if result else 0,
        "remaining_turns": len(budget.turns),
    }


@DBOS.workflow()
def run_greenhouse_scenario(
    goal_id: str,
    goal_text: str,
    db_path: str,
    migrations_dir: str,
    amh_actuate_bin: str,
    device_action_id: str,
    device_host: str,
    device_port: int,
    forward: str,
    read_state: str,
) -> dict[str, Any]:
    """Top-level durable workflow for the full V0 scenario. Crash-safe: if
    the process dies after step 4's actuation is durably recorded (either
    by amh-actuate's own SQLite commit, or by DBOS's step-completion
    record) but before this function returns, resuming replays only the
    remaining steps — see test_greenhouse_e2e.py's restart-survival test.
    """
    # Steps 1-2: decompose + isolated sub-agent execution
    tasks = decompose_goal(goal_id, goal_text, db_path)
    handles = [DBOS.start_workflow(run_subagent, t["task_id"], t["objective"], db_path) for t in tasks]
    gathered = [h.get_result() for h in handles]
    summary = synthesize(goal_id, gathered, db_path)

    # Step 3: context compaction under sustained polling (in-process, not durable —
    # see module docstring)
    compaction = simulate_overnight_polling()

    # Step 4: autonomous reversible actuation — no ApprovalGate, verified inverse
    actuation_result = actuate_device(
        amh_actuate_bin,
        db_path,
        migrations_dir,
        device_action_id,
        device_host,
        device_port,
        forward,
        read_state,
        insecure_skip_host_key_verify=True,
    )

    return {
        "summary": summary,
        "compaction": compaction,
        "actuation_result": actuation_result,
    }
