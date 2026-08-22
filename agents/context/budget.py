"""Per-agent token budget manager, per docs/AMH-SPECIFICATION.md §3.

Tracks how much of the context window is used, enforces a per-tool-result
cap (defaulting to 25k tokens, matching Claude Code's product default), and
reports when the compaction threshold is crossed so the compactor (§3.2,
compactor.py) can act.

Two-tier token counting, matching the fail-honest (not fail-fake) pattern
daemon/credentials and daemon/authn already use elsewhere in this
codebase: when a real model client is configured (agents/context/llm.py),
BudgetManager.count_tokens should be set to that client's real
provider-side token count — the actual number the model will see, not an
estimate. approximate_token_count below is the explicit fallback for when
no model client is configured (no API key, or a caller that only needs a
cheap local estimate) — it is not a stand-in for the real thing pretending
to be one; count_tokens defaults to it only until a caller supplies the
real counter.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Callable


def approximate_token_count(text: str) -> int:
    """Local, offline fallback: ~4 chars/token, the commonly-cited
    English-text ratio for GPT/Claude-family tokenizers. Deliberately not
    an attempt at a more accurate local heuristic — English word-count-based
    ratios are the same order of crudeness as chars/4, and a materially
    better count means the real provider tokenizer, not a fancier guess.
    Good enough to trigger budget and compaction thresholds at roughly the
    right point; not a substitute for the real count when exact accounting
    matters (billing, hard provider-side limits) — see this module's
    top-level doc comment for how to supply the real one."""
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
