"""Wires the memory projections (docs/AMH-SPECIFICATION.md §8) into goal
pursuit (goal.py's decompose_goal/synthesize, shared by pursue_goal and
run_greenhouse_scenario) — this is the first real consumer of episodic
memory (Hindsight) and semantic/entity memory (Graphiti/Neo4j); until now
both existed only as unconsumed infrastructure.

recall_context/retain_outcome are best-effort: Hindsight and Graphiti are
optional enrichment an operator may not have deployed. Each degrades to a
no-op leg when its own configuration is absent (HINDSIGHT_BASE_URL,
GRAPHITI_NEO4J_URI/GRAPHITI_KUZU_DB_PATH unset) — the same way
ModelClient.embed() already treats an unconfigured embedding model as
"not this deployment's problem" rather than an error. A real failure once
configured (a reachable-but-erroring Hindsight/Neo4j) is never swallowed
here — only "not configured" is optional; everything else propagates and
fails the step, same as every other model-provider call in this codebase.

Working memory (agents/memory/working.py) is wired separately, directly
into goal.do_subagent_work, since it projects one run's own DB state
rather than needing a network call to an external service.
"""

from __future__ import annotations

import asyncio
import os
from datetime import datetime, timezone

from dbos import DBOS

from context.llm import from_env
from memory.episodic import HindsightNotConfiguredError, hindsight_from_env
from memory.graph import GraphDriverNotConfiguredError, build_graphiti


def _bank_id() -> str:
    """The Hindsight bank / Graphiti group_id memory is partitioned under.
    A single habitat-wide partition by default — AMH_MEMORY_BANK_ID lets an
    operator running multiple habitats against a shared Hindsight/Neo4j
    deployment separate them."""
    return os.environ.get("AMH_MEMORY_BANK_ID", "amh-habitat")


def _embedding_dim() -> int:
    """Must match the real output dimensionality of the daemon-routed
    embedding model — see memory/graph_llm.py's DaemonGraphitiEmbedderClient
    docstring. Not guessed: an operator enabling Graphiti sets this to their
    actual embedding model's dimension."""
    return int(os.environ.get("GRAPHITI_EMBEDDING_DIM", "1024"))


async def _graphiti_search(model_client, query_text: str) -> list[str]:
    graphiti = build_graphiti(model_client, embedding_dim=_embedding_dim())
    try:
        edges = await graphiti.search(query_text)
        return [e.fact for e in edges if e.fact]
    finally:
        await graphiti.close()


async def _graphiti_add_episode(model_client, name: str, episode_body: str, source_description: str) -> None:
    graphiti = build_graphiti(model_client, embedding_dim=_embedding_dim())
    try:
        await graphiti.add_episode(
            name=name,
            episode_body=episode_body,
            source_description=source_description,
            reference_time=datetime.now(timezone.utc),
            group_id=_bank_id(),
        )
    finally:
        await graphiti.close()


@DBOS.step()
def recall_context(query_text: str, daemon_api_base_url: str, agent_token: str) -> str:
    """Best-effort recall from episodic (Hindsight) and semantic/entity
    (Graphiti) memory, combined into one text block a caller can prepend to
    a model prompt. Returns "" if neither is configured, or neither finds
    anything relevant — never fabricates a result."""
    blocks: list[str] = []

    try:
        hindsight = hindsight_from_env()
    except HindsightNotConfiguredError:
        pass
    else:
        recalled = hindsight.recall(bank_id=_bank_id(), query=query_text)
        facts = "\n".join(r.text for r in recalled.results if r.text)
        if facts:
            blocks.append(f"Relevant past episodes:\n{facts}")

    model_client = from_env(daemon_api_base_url, agent_token)
    try:
        graph_facts = asyncio.run(_graphiti_search(model_client, query_text))
    except GraphDriverNotConfiguredError:
        pass
    else:
        if graph_facts:
            blocks.append("Relevant known facts:\n" + "\n".join(graph_facts))

    return "\n\n".join(blocks)


@DBOS.step()
def retain_outcome(goal_text: str, summary: str, daemon_api_base_url: str, agent_token: str) -> None:
    """Best-effort retention into episodic (Hindsight) and semantic/entity
    (Graphiti) memory. Same optional-if-unconfigured semantics as
    recall_context."""
    content = f"Goal: {goal_text}\nOutcome: {summary}"

    try:
        hindsight = hindsight_from_env()
    except HindsightNotConfiguredError:
        pass
    else:
        hindsight.retain(bank_id=_bank_id(), content=content, timestamp=datetime.now(timezone.utc))

    model_client = from_env(daemon_api_base_url, agent_token)
    try:
        asyncio.run(
            _graphiti_add_episode(
                model_client,
                name=f"goal-outcome-{datetime.now(timezone.utc).isoformat()}",
                episode_body=content,
                source_description="goal outcome",
            )
        )
    except GraphDriverNotConfiguredError:
        pass
