"""Per-agent token budget manager, per docs/AMH-SPECIFICATION.md §3.

Tracks how much of the context window is used, enforces a per-tool-result
cap (defaulting to 25k tokens, matching Claude Code's product default), and
reports when the compaction threshold is crossed so the compactor (§3.2,
compactor.py) can act.

V0 token counting is a documented approximation (chars // 4) rather than a
real tokenizer — plugging in a model-specific tokenizer is a drop-in swap
via the `count_tokens` callable, not a redesign.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Callable


def approximate_token_count(text: str) -> int:
    """V0 approximation: ~4 chars/token, the commonly-cited English-text
    ratio for GPT/Claude-family tokenizers. Good enough to trigger budget
    and compaction thresholds at roughly the right point; not a substitute
    for a real tokenizer when exact accounting matters (billing, hard
    provider-side limits)."""
    return max(1, len(text) // 4)


@dataclass
class Turn:
    role: str
    content: str
    tokens: int
    truncated: bool = False


@dataclass
class BudgetManager:
    window_budget: int = 180_000
    compact_at: float = 0.70
    tool_result_cap: int = 25_000
    count_tokens: Callable[[str], int] = approximate_token_count
    turns: list[Turn] = field(default_factory=list)

    @property
    def used_tokens(self) -> int:
        return sum(t.tokens for t in self.turns)

    @property
    def fraction_used(self) -> float:
        return self.used_tokens / self.window_budget

    @property
    def over_compact_threshold(self) -> bool:
        return self.fraction_used >= self.compact_at

    def cap_tool_result(self, content: str) -> tuple[str, bool]:
        """Truncates a single tool result to tool_result_cap tokens,
        returning (possibly-truncated content, truncated flag) — mirrors
        DeepAgents' `truncated` flag so callers can surface it to the
        model rather than silently dropping data."""
        tokens = self.count_tokens(content)
        if tokens <= self.tool_result_cap:
            return content, False
        # Approximate: cut proportionally to the token overage, since we
        # don't have per-token offsets without a real tokenizer.
        keep_chars = self.tool_result_cap * 4
        return content[:keep_chars], True

    def add_turn(self, role: str, content: str, is_tool_result: bool = False) -> Turn:
        if is_tool_result:
            content, truncated = self.cap_tool_result(content)
        else:
            truncated = False
        turn = Turn(role=role, content=content, tokens=self.count_tokens(content), truncated=truncated)
        self.turns.append(turn)
        return turn

    def replace_turns(self, new_turns: list[Turn]) -> None:
        """Used by the compactor to swap in a compacted history."""
        self.turns = new_turns
