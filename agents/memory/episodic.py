"""Episodic memory and knowledge retrieval projections
(docs/AMH-SPECIFICATION.md §8) — Hindsight (github.com/vectorize-io/hindsight),
self-hosted and schema-isolated within the same PostgreSQL cluster as the
rest of AMH's authoritative state (§1 decision 4: Postgres is the sole,
day-one authoritative store — not a second database). Hindsight owns
retain/recall/reflect directly; this module does not re-wrap them, only
constructs the client (matching memory/graph.py's role for Graphiti).

Hindsight is a separate service process (`pip install hindsight-api-slim`;
its own FastAPI server, not a Python library this codebase imports —
NOT a dependency of agents/ itself, same as Postgres and Neo4j are
"bring your own running instance" infrastructure this codebase only holds
a connection string for). agents/ depends only on hindsight-client, a
thin HTTP client with no ML dependencies of its own.

Every model call Hindsight's server makes (fact extraction, embeddings)
is configured, at the Hindsight server's own deployment, to route through
daemon/api's OpenAI-compatible facade (/v1/openai/*) rather than holding
a model-provider credential directly — see .env.example's Hindsight
section for the exact HINDSIGHT_API_LLM_*/HINDSIGHT_API_EMBEDDINGS_*
variables. Verified for real (not assumed) against a live, self-hosted
hindsight-api-slim instance backed by a real Postgres+pgvector database
and a stand-in HTTP server playing the daemon's OpenAI-compatible facade:
a real retain() call performed real fact extraction and a real recall()
call performed real hybrid retrieval, over genuinely plain-text chat
completions with the JSON schema described in the prompt body — Hindsight's
openai_compatible provider never sent an OpenAI "tools"/"tool_choice"/
"response_format" field for either operation, so daemon/api/
controlplane.go's facade (plain-text completions only, no function-calling
support) is sufficient; it does not need to fabricate tool_calls.

HINDSIGHT_API_KEY, if the operator's deployment requires one, is Hindsight's
own service credential (like a database password) — internal infrastructure
this process holds directly via env, not a model-provider secret requiring
daemon custody.
"""

from __future__ import annotations

import os

from hindsight_client import Hindsight


class HindsightNotConfiguredError(Exception):
    """HINDSIGHT_BASE_URL is not set."""


def hindsight_from_env() -> Hindsight:
    base_url = os.environ.get("HINDSIGHT_BASE_URL", "").strip()
    if not base_url:
        raise HindsightNotConfiguredError("HINDSIGHT_BASE_URL is not set — no Hindsight instance is configured")
    api_key = os.environ.get("HINDSIGHT_API_KEY", "").strip() or None
    return Hindsight(base_url=base_url, api_key=api_key)
