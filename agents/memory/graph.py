"""Semantic and entity memory projections (docs/AMH-SPECIFICATION.md §8) —
Neo4j via Graphiti (getzep/graphiti). Graphiti owns the bi-temporal claim
graph directly: `Graphiti.add_episode()` extracts entities and edges from
raw text/JSON via the LLM and writes them with `created_at`/`expired_at`/
`valid_at`/`invalid_at` and automatic contradiction resolution
(`resolve_edge_contradictions`) already built in, and `Graphiti.search()`
already does hybrid (BM25 + cosine + graph-distance reranking) retrieval
over that graph — so this module does not re-wrap those two calls behind a
narrower custom API; callers use the real `Graphiti` object's own methods
directly.

What this module owns is construction: wiring Graphiti's LLMClient/
EmbedderClient interfaces to the daemon's inference seam (see
memory/graph_llm.py) and selecting the graph driver.

Neo4j is the adopted production backend. Kuzu (an embedded, in-process
graph database — no external service, pip-installable) is available only
as an explicit opt-in for sandbox/test environments where a real Neo4j
instance isn't reachable; it is not a silent default; production
deployments must set the Neo4j environment variables.

Kuzu support in graphiti-core is itself deprecated upstream ("no longer
maintained... migrate to Neo4j or FalkorDB") and, in the installed
version, cannot actually create an edge through Graphiti's normal
dedup path (its schema never creates the full-text index that path's
duplicate-edge check queries — see tests/test_memory_graph.py's module
docstring) — usable only for the narrower verification that test file
covers, not as a real edge-graph substitute for Neo4j.
"""

from __future__ import annotations

import os

from graphiti_core.driver.driver import GraphDriver
from graphiti_core.graphiti import Graphiti

from context.llm import ModelClient
from memory.graph_llm import DaemonGraphitiCrossEncoderClient, DaemonGraphitiEmbedderClient, DaemonGraphitiLLMClient


class GraphDriverNotConfiguredError(Exception):
    """Neither GRAPHITI_NEO4J_URI nor GRAPHITI_KUZU_DB_PATH is set."""


def graph_driver_from_env() -> GraphDriver:
    """Selects the graph backend from environment variables.

    Production: GRAPHITI_NEO4J_URI, GRAPHITI_NEO4J_USER,
    GRAPHITI_NEO4J_PASSWORD (GRAPHITI_NEO4J_DATABASE defaults to "neo4j").

    Sandbox/test opt-in only: GRAPHITI_KUZU_DB_PATH (a filesystem path, or
    ":memory:" for a non-persistent in-process graph) — used when
    GRAPHITI_NEO4J_URI is unset.
    """
    neo4j_uri = os.environ.get("GRAPHITI_NEO4J_URI", "").strip()
    if neo4j_uri:
        from graphiti_core.driver.neo4j_driver import Neo4jDriver

        return Neo4jDriver(
            uri=neo4j_uri,
            user=os.environ.get("GRAPHITI_NEO4J_USER", "").strip() or None,
            password=os.environ.get("GRAPHITI_NEO4J_PASSWORD", "").strip() or None,
            database=os.environ.get("GRAPHITI_NEO4J_DATABASE", "neo4j").strip(),
        )

    kuzu_db_path = os.environ.get("GRAPHITI_KUZU_DB_PATH", "").strip()
    if kuzu_db_path:
        from graphiti_core.driver.kuzu_driver import KuzuDriver

        return KuzuDriver(db=kuzu_db_path)

    raise GraphDriverNotConfiguredError(
        "no graph driver configured — set GRAPHITI_NEO4J_URI (production) or "
        "GRAPHITI_KUZU_DB_PATH (sandbox/test opt-in only)"
    )


def build_graphiti(
    model_client: ModelClient,
    embedding_dim: int,
    driver: GraphDriver | None = None,
    small_model: str | None = None,
) -> Graphiti:
    """Builds a real Graphiti instance backed by the daemon's inference
    seam for both generation and embedding. driver defaults to
    graph_driver_from_env() when not given explicitly."""
    return Graphiti(
        graph_driver=driver if driver is not None else graph_driver_from_env(),
        llm_client=DaemonGraphitiLLMClient(model_client, small_model=small_model),
        embedder=DaemonGraphitiEmbedderClient(model_client, embedding_dim=embedding_dim),
        cross_encoder=DaemonGraphitiCrossEncoderClient(model_client),
    )
