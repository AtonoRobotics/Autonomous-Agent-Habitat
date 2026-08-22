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
import re
from dataclasses import asdict, dataclass, field
from typing import Callable

from .budget import BudgetManager, Turn
from .llm import ModelClient

# The 8 fields docs/AMH-SPECIFICATION.md §7 rule 6 requires compaction to
# preserve ("Compaction preserves governing constraints, goal, completion
# predicate, unresolved decisions, failures, uncertainty, active plan, and
# artifact references") — see acceptance invariant #8. Every summarize
# strategy returns one of these, not a bare string, so a caller can always
# find where to look for a required field, even when its value is None
# because a given strategy has no way to recover it — see
# extractive_summarize's doc comment for which fields that honestly is,
# for an offline, non-semantic strategy.
@dataclass
class Checkpoint:
    goal: str | None = None
    governing_constraints: str | None = None
    completion_predicate: str | None = None
    unresolved_decisions: list[str] = field(default_factory=list)
    failures: list[str] = field(default_factory=list)
    uncertainty: str | None = None
    active_plan: str | None = None
    artifact_references: list[str] = field(default_factory=list)

    def to_json(self) -> str:
        return json.dumps(asdict(self))


SummarizeFn = Callable[[list[Turn], "str | None"], Checkpoint]

_SUMMARIZE_SYSTEM_PROMPT = """Summarize this conversation history for a model that must continue the \
task without access to the original turns.

Respond with ONLY a JSON object with these fields, per docs/AMH-SPECIFICATION.md \
§7 rule 6: "goal" (string), "governing_constraints" (string or null), \
"completion_predicate" (string or null), "unresolved_decisions" (array of \
strings), "failures" (array of strings), "uncertainty" (string or null), \
"active_plan" (string or null), "artifact_references" (array of strings). \
Use null for a field you genuinely cannot determine from the transcript — \
never fabricate a value. Do not include any text outside the JSON object."""

# Syntactic-only, no semantic understanding: a URI (scheme://...) or an
# absolute filesystem path — real, mechanically recoverable references a
# tool result would actually contain (e.g. VFS's own "wrote {path}"
# convention, or an "artifact://..." reference), not a guess. Matched by
# whitespace-splitting rather than one regex scanning raw text: a \b
# anchor in front of a bare "/" matches at every internal path-segment
# boundary too (word-char "e" followed by non-word "/" is itself a word
# boundary), not just a token's start — silently truncating
# "/workspace/notes/plan.md" down to "/notes/plan.md". Token-by-token
# sidesteps that entirely.
_URI_SCHEME_PATTERN = re.compile(r"^[a-zA-Z][a-zA-Z0-9+.-]*://")
_REFERENCE_STRIP_CHARS = ".,;:()[]\"'"

# agentic_loop.py's own established convention for a failed tool call
# (_run_agentic_loop_async's mcp/builtin-tool except branches): the turn
# content is literally prefixed "error: ".
_ERROR_TURN_PREFIX = "error: "


def extractive_summarize(turns: list[Turn], objective: str | None = None) -> Checkpoint:
    """Real, permanent, offline strategy — not a model call, so it can
    only genuinely, mechanically recover what's syntactically present:
    `goal` from the caller-supplied objective (budget.turns never
    contains it — the harness passes it separately as the system prompt,
    never as a turn), `failures` from turns already prefixed "error: ",
    and `artifact_references` via a real path/URI scan. The remaining
    fields (governing_constraints, completion_predicate,
    unresolved_decisions, uncertainty, active_plan) require semantic
    understanding this strategy doesn't have, so they stay honestly None/
    empty rather than fabricated — swap via
    Compactor(summarize=llm_summarize(client)) for a model-authored
    checkpoint that can actually populate them."""
    failures = [t.content[len(_ERROR_TURN_PREFIX) :] for t in turns if t.content.startswith(_ERROR_TURN_PREFIX)]
    artifact_references: list[str] = []
    for t in turns:
        for token in t.content.split():
            candidate = token.strip(_REFERENCE_STRIP_CHARS)
            if not (_URI_SCHEME_PATTERN.match(candidate) or candidate.startswith("/")):
                continue
            if candidate not in artifact_references:
                artifact_references.append(candidate)
    return Checkpoint(goal=objective, failures=failures, artifact_references=artifact_references)


def llm_summarize(client: ModelClient) -> SummarizeFn:
    """Returns a SummarizeFn backed by a real model call through client —
    unlike extractive_summarize, a real model has the semantic
    understanding to genuinely populate every Checkpoint field, not just
    the syntactically-recoverable ones. Raises
    context.llm.ModelNotConfiguredError (propagated from the call) if the
    provider call fails, or ValueError if the model's response is not the
    requested JSON shape (mirrors workflows/goal.py's decompose_goal) —
    never falls back to extractive_summarize silently; a caller wanting
    that fallback must catch the error itself and choose it explicitly."""

    def _summarize(turns: list[Turn], objective: str | None = None) -> Checkpoint:
        transcript = "\n\n".join(f"[{t.role}] {t.content}" for t in turns)
        user_content = f"Goal: {objective}\n\n{transcript}" if objective else transcript
        response_text = client.complete(system=_SUMMARIZE_SYSTEM_PROMPT, messages=[{"role": "user", "content": user_content}])
        try:
            parsed = json.loads(response_text)
        except json.JSONDecodeError as e:
            raise ValueError(f"llm_summarize: model response was not valid JSON: {response_text!r}") from e
        if not isinstance(parsed, dict):
            raise ValueError(f"llm_summarize: model response was not a JSON object: {parsed!r}")
        try:
            return Checkpoint(**parsed)
        except TypeError as e:
            raise ValueError(f"llm_summarize: model response had unexpected fields: {parsed!r}") from e

    return _summarize


@dataclass
class CompactionResult:
    summary: Checkpoint
    kept_verbatim: list[Turn]
    turns_compacted: int


class Compactor:
    def __init__(self, keep_recent_turns_raw: int = 3, summarize: SummarizeFn = extractive_summarize):
        self.keep_recent_turns_raw = keep_recent_turns_raw
        self.summarize = summarize

    def compact(self, budget: BudgetManager, objective: str | None = None) -> CompactionResult | None:
        """Compacts budget.turns in place if the threshold is crossed;
        returns None (no-op) otherwise. Cache-stable prefix discipline
        (§3.5): the summary turn always goes first, then verbatim recent
        turns — a stable shape regardless of how many times compaction
        runs, so the prompt prefix stays append-only-shaped even across
        repeated compactions. objective is threaded through to the
        summarize strategy so it can populate Checkpoint.goal — budget.turns
        never contains it (the harness passes it separately, never as a
        turn)."""
        if not budget.over_compact_threshold:
            return None
        if len(budget.turns) <= self.keep_recent_turns_raw:
            return None

        split = len(budget.turns) - self.keep_recent_turns_raw
        to_compact, keep_verbatim = budget.turns[:split], budget.turns[split:]

        checkpoint = self.summarize(to_compact, objective)
        checkpoint_json = checkpoint.to_json()
        summary_turn = Turn(
            role="system",
            content=checkpoint_json,
            tokens=budget.count_tokens(checkpoint_json),
        )

        budget.replace_turns([summary_turn, *keep_verbatim])

        return CompactionResult(
            summary=checkpoint,
            kept_verbatim=keep_verbatim,
            turns_compacted=len(to_compact),
        )
