# Autonomous Multi-Agent Habitat (AMH)

AMH is a 24/7 runtime habitat for autonomous agents. It supplies durable work, context, memory, tools, workers, extension composition, recovery, and stable interfaces for domain applications.

It is not a governance application, security application, physical-device stack, or domain product.

## Architecture

- **Python cognition workers:** model calls, context, retrieval, planning, subordinate agents, coder agents, and offline improvement.
- **Go habitat daemon:** service supervision, extension hosting, isolation, local transport, MCP gateway, and connector process I/O.
- **DBOS Transact + SQLite:** sole V1 durable workflow engine and authoritative embedded state. PostgreSQL is the scale path.
- **Cordis composition:** reversible software effects plus dependency-aware activation and teardown.
- **Small core, swappable extensions:** providers, connectors, harness policies, memory implementations, and domains remain replaceable.

## Core versus extensions

The core owns durable execution, supervision, context/artifacts, domain-neutral ontology, generic policy hooks, extension lifecycle, interoperability, observability, and eval-gated promotion.

Extensions own the semantics they introduce. A Physical AI extension—not AMH core—owns devices, physical actions, inverse verification, interlocks, safe states, physical spatial data, and physical safety policy.

Reversibility is a generic property that policy may use as a gate. For an extension action, the owning extension defines and verifies that property and selects any recovery; AMH durably records and enforces the resulting decision without interpreting the domain.

## Runtime ownership

- DBOS owns durable workflow transitions.
- SQLite owns persisted truth.
- The Go daemon supervises processes and delivers triggers; it does not independently retry or complete DBOS work.
- NATS is optional scale infrastructure, not a V1 core dependency.
- MCP 2026-07-28 is the tool interoperability baseline.
- A2A 1.0 is the external agent interoperability baseline.

## Repository

```text
daemon/       Go daemon and extension host
agents/       Python cognition workers
contracts/    Normative core JSON Schemas
docs/         Governing architecture and decisions
sdk/          Extension and domain-app SDKs
store/        Embedded store implementation
migrations/   Ordered schema migrations
evals/        Acceptance and self-improvement evaluations
deploy/       Linux and Windows service packaging
```

Read [the governing specification](docs/AMH-SPECIFICATION.md) before implementation. Domain extensions must consume the contracts in `contracts/` and must not add domain entities to the AMH core ontology.

**Authoritative revision:** 10 — 2026-08-21.
