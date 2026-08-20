from __future__ import annotations

from context.budget import BudgetManager
from context.compactor import Compactor


def test_budget_tracks_usage_fraction():
    budget = BudgetManager(window_budget=100)
    budget.add_turn("user", "a" * 40)  # ~10 tokens at chars//4
    assert budget.used_tokens == 10
    assert budget.fraction_used == 0.10
    assert not budget.over_compact_threshold


def test_budget_crosses_compact_threshold():
    budget = BudgetManager(window_budget=100, compact_at=0.70)
    budget.add_turn("user", "a" * 400)  # 100 tokens > 70% of 100
    assert budget.over_compact_threshold


def test_tool_result_cap_truncates_and_flags():
    budget = BudgetManager(tool_result_cap=10)  # 10 tokens = 40 chars
    content = "x" * 1000
    turn = budget.add_turn("tool", content, is_tool_result=True)
    assert turn.truncated is True
    assert turn.tokens <= 10
    assert len(turn.content) == 40


def test_tool_result_under_cap_is_not_truncated():
    budget = BudgetManager(tool_result_cap=1000)
    turn = budget.add_turn("tool", "short result", is_tool_result=True)
    assert turn.truncated is False
    assert turn.content == "short result"


def test_compactor_noop_below_threshold():
    budget = BudgetManager(window_budget=1_000_000, compact_at=0.70)
    budget.add_turn("user", "hello")
    result = Compactor().compact(budget)
    assert result is None
    assert len(budget.turns) == 1


def test_compactor_summarizes_oldest_keeps_recent_verbatim():
    budget = BudgetManager(window_budget=100, compact_at=0.70)
    for i in range(10):
        budget.add_turn("user" if i % 2 == 0 else "assistant", f"turn {i} " * 5)

    assert budget.over_compact_threshold
    original_turns = list(budget.turns)

    compactor = Compactor(keep_recent_turns_raw=3)
    result = compactor.compact(budget)

    assert result is not None
    assert result.turns_compacted == 7
    # New shape: [summary, *3 verbatim recent turns]
    assert len(budget.turns) == 4
    assert budget.turns[0].role == "system"
    assert budget.turns[1:] == original_turns[-3:]


def test_compaction_is_idempotent_shape_across_repeated_runs():
    """After compaction, the turn list is [summary, verbatim...] — running
    compact() again (once enough new turns accumulate) must produce the
    same stable shape, not a growing chain of nested summaries, per the
    cache-stable-prefix discipline in §3."""
    budget = BudgetManager(window_budget=100, compact_at=0.70)
    for i in range(10):
        budget.add_turn("user", f"turn {i} " * 5)
    compactor = Compactor(keep_recent_turns_raw=3)
    compactor.compact(budget)
    assert len(budget.turns) == 4

    for i in range(10, 20):
        budget.add_turn("user", f"turn {i} " * 5)
    compactor.compact(budget)
    assert budget.turns[0].role == "system"
    assert len(budget.turns) == 4
