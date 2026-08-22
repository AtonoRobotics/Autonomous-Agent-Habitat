"""Context compactor, per docs/AMH-SPECIFICATION.md §3.

Triggers when BudgetManager.over_compact_threshold is true (default 70% of
window). Hierarchically summarizes the oldest turns to structured JSON,
retains the most recent K turns verbatim (an AMH design choice, to preserve
the model's formatting rhythm), and emits a compaction event to the durable
log for replay — matching the greenhouse scenario's step 3 in Artifact H.

Two summarization strategies, same fail-honest split as context/llm.py and
budget.py's token counting: extractive_summarize is a real, permanent,
offline strategy (concatenate + truncate to structured JSON) — not a stand-in
for a model call, just a strategy that never needs a network. llm_summarize
is the model-driven strategy, wrapping a real context.llm.ModelClient; use
it via Compactor(summarize=llm_summarize(client)). Compactor's default
stays extractive_summarize because it requires no configuration to run —
callers that want a real LLM-authored summary opt in explicitly by
supplying a configured client.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Callable

from .budget import BudgetManager, Turn
from .llm import ModelClient

SummarizeFn = Callable[[list[Turn]], str]

_SUMMARIZE_SYSTEM_PROMPT = """Summarize this conversation history for a model that must continue the \
task without access to the original turns. Preserve: the governing goal, \
completion predicate, unresolved decisions, failures encountered, and any \
artifact references. Be concise; this summary replaces the original turns \
in the model's context window."""


def extractive_summarize(turns: list[Turn]) -> str:
    """Real, permanent, offline strategy: a structured, lossy-but-legible
    extractive summary, not a model call. This is the default because it
    needs no configuration — swap via Compactor(summarize=llm_summarize(client))
    for a model-authored summary once a real client is available."""
    return json.dumps(
        {
            "compacted_turn_count": len(turns),
            "roles": [t.role for t in turns],
            "excerpt": " / ".join(t.content[:80] for t in turns)[:2000],
        }
    )


def llm_summarize(client: ModelClient) -> SummarizeFn:
    """Returns a SummarizeFn backed by a real model call through client.
    Raises context.llm.ModelNotConfiguredError (propagated from the call)
    if the provider call fails — never falls back to extractive_summarize
    silently; a caller wanting that fallback must catch the error itself
    and choose it explicitly."""

    def _summarize(turns: list[Turn]) -> str:
        transcript = "\n\n".join(f"[{t.role}] {t.content}" for t in turns)
        return client.complete(system=_SUMMARIZE_SYSTEM_PROMPT, messages=[{"role": "user", "content": transcript}])

    return _summarize


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
