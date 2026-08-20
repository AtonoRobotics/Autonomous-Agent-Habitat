"""Context compactor, per docs/AMH-SPECIFICATION.md §3.

Triggers when BudgetManager.over_compact_threshold is true (default 70% of
window). Hierarchically summarizes the oldest turns to structured JSON,
retains the most recent K turns verbatim (an AMH design choice, to preserve
the model's formatting rhythm), and emits a compaction event to the durable
log for replay — matching the greenhouse scenario's step 3 in Artifact H.

V0 summarization is extractive (concatenate + truncate), not model-driven —
producing a real LLM-authored summary needs a model call this session has
no credentials for. The mechanism (threshold trigger, keep-recent-verbatim,
durable event emission) is what's under test here; the summarization
quality is a swappable strategy.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Callable

from .budget import BudgetManager, Turn

SummarizeFn = Callable[[list[Turn]], str]


def extractive_summarize(turns: list[Turn]) -> str:
    """V0 default: a structured, lossy-but-legible extractive summary —
    not a model call. Swap via Compactor(summarize=...) once an LLM
    summarization step exists."""
    return json.dumps(
        {
            "compacted_turn_count": len(turns),
            "roles": [t.role for t in turns],
            "excerpt": " / ".join(t.content[:80] for t in turns)[:2000],
        }
    )


@dataclass
class CompactionResult:
    summary: str
    kept_verbatim: list[Turn]
    turns_compacted: int


class Compactor:
    def __init__(self, keep_recent_turns_raw: int = 3, summarize: SummarizeFn = extractive_summarize):
        self.keep_recent_turns_raw = keep_recent_turns_raw
        self.summarize = summarize

    def compact(self, budget: BudgetManager) -> CompactionResult | None:
        """Compacts budget.turns in place if the threshold is crossed;
        returns None (no-op) otherwise. Cache-stable prefix discipline
        (§3.5): the summary turn always goes first, then verbatim recent
        turns — a stable shape regardless of how many times compaction
        runs, so the prompt prefix stays append-only-shaped even across
        repeated compactions."""
        if not budget.over_compact_threshold:
            return None
        if len(budget.turns) <= self.keep_recent_turns_raw:
            return None

        split = len(budget.turns) - self.keep_recent_turns_raw
        to_compact, keep_verbatim = budget.turns[:split], budget.turns[split:]

        summary_text = self.summarize(to_compact)
        summary_turn = Turn(
            role="system",
            content=summary_text,
            tokens=budget.count_tokens(summary_text),
        )

        budget.replace_turns([summary_turn, *keep_verbatim])

        return CompactionResult(
            summary=summary_text,
            kept_verbatim=keep_verbatim,
            turns_compacted=len(to_compact),
        )
