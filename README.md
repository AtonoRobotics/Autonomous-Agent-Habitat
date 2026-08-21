# Autonomous Multi-Agent Habitat (AMH)

A platform core for autonomous agents doing real work under bounded authority. Fully autonomous, running 24/7 — not a governance app, not a security app, not a physical-robot stack. Domain-specific applications (including Physical AI) are built **on** AMH, not into it.

**Architecture (v9):** Python agent/LLM layer (DeepAgents patterns reimplemented natively on DBOS) + Go control-plane daemon, DBOS Transact on SQLite, embedded hybrid KG + vector store, **small hard kernel + swappable capability modules**, reversible software capability composition (Cordis-inspired), eval-gated self-improvement, and stable seams for domain extensions.

## Scope correction (v9)

Physical device safety, actuation interlocks, SSH/OPC-UA/MQTT device policy, and earned autonomy for physical effects are **not** core AMH concerns. They belong in a **Physical AI extension** that registers connectors, action policies, and interlocks through AMH’s module and policy seams. AMH provides the durable habitat, agent harness, memory, and generic authorization hooks — not domain safety policy.

## Layout

```
daemon/           Go control plane (single binary per OS)
├── cmd/amh-daemon        systemd/Windows-service entry
├── supervisor/           OTP-style supervision tree
├── scheduler/            cron/triggers/event sources
├── bus/                  embedded NATS + blackboard
├── mcp/                  MCP client + server (stdio, Streamable HTTP)
├── connectors/           generic connector runtime (protocol adapters as modules)
├── policy/               generic ApprovalGate / policy enforcement hooks
└── health/               watchdog, circuit breakers, bulkheads

agents/           Python agent/LLM layer (uv-managed)
├── harness/              deep-agent patterns: planning, VFS, subagents, middleware
├── coder/                CodeAct sandbox loop (+ optional PTC/Code mode)
├── context/              budget mgr, compactor, offload
├── memory/               5-tier + bi-temporal KG
├── kb/                   hybrid RAG (sqlite-vec + FTS5 + graph)
├── modules/              capability lifecycle: register, start, stop, unload
├── selfimprove/          GEPA/ACE/Voyager + eval-gate
└── workflows/            DBOS durable workflows & activities

contracts/        SHARED public interfaces
├── ontology.schema.json
├── envelope.schema.json
├── manifests/
└── proto/                daemon↔agent gRPC

sdk/              domain-app & extension SDK (stable public API)
├── python/               amh_sdk (Protocols, seams)
└── ts/                   @amh/sdk (interfaces)

extensions/       (optional in-tree examples; production packs may live out-of-tree)
└── physical-ai/          device connectors, physical action policy, SafetyCase — NOT core

store/            DDL, migrations
migrations/       ordered SQL migrations
evals/            eval suites, canary configs
deploy/           systemd unit, Windows service, installers
```

## Getting started

**Read first:** `docs/AMH-SPECIFICATION.md` — architecture, kernel vs modules, design rationale, V0/V1 roadmap.

**V0 (walking skeleton):** Go daemon supervisor + Python agent layer on DBOS/SQLite, MCP client, context budget manager, module lifecycle stub, OTel tracing. A non-physical demo workflow (goal → plan → tool use → durable replay) survives daemon restart.

**V1 (autonomous habitat):** orchestrator/worker multi-agent, full 5-tier memory + bi-temporal KG, coder agent + sandbox, self-improvement (GEPA/ACE/Voyager behind eval/canary/promote), capability-level self-healing, Connector SDK, MCP server, extension/plugin model with reversible software effects. Physical AI ships as a separate extension pack.

## Key architectural decisions (v9)

| # | Subsystem | Decision | Why |
|---|-----------|----------|-----|
| 1 | Runtime | Python + Go | Deepest agent ecosystem + single-binary daemon, native Windows |
| 2 | Durable execution | DBOS Transact (SQLite) | In-process library, no separate server, official Windows support |
| 3 | Architecture shape | **Small hard kernel + swappable modules** | Rapid evolution without baking features into the core |
| 4 | Harness | Native DeepAgents patterns on DBOS (not the package) | Patterns are right; package durability collides with DBOS |
| 5 | Composition | Cordis-inspired reversible effects + capability seams | Safe unload/rollback of software modules; DeepSeek Harness as teacher |
| 6 | Context | KV-cache-stable + FS offload + sub-agent isolation | Anthropic/Manus/Cognition convergence |
| 7 | Memory | 5-tier CoALA + bi-temporal KG | Standard cognitive taxonomy; Graphiti-style fact invalidation |
| 8 | Spatial | Optional core primitives (R-tree Location); **algorithms & safety in extensions** | Stable interface without domain policy in the kernel |
| 9 | Physical safety | **Out of core — Physical AI extension** | Domain concern; keeps AMH usable for non-physical autonomy |
| 10 | Self-mod safety | Independent tamper-proof instrumentation + eval/canary | Prevent reward-hacking; agent cannot edit its own eval tooling |
| 11 | Policy | Generic ApprovalGate / policy hooks in core | Extensions supply domain policy (including physical interlocks) |

## Kernel vs modules

**Kernel (stable, hard to bypass):** durable execution, supervision/health, reconstructible trajectory log, core ontology + store, module lifecycle, generic policy enforcement surface, public SDK contracts.

**Modules (swappable):** harness variants, compaction policies, skills/prompts, self-improvement optimizers, model providers, connectors, RAG backends, domain packs (including Physical AI).

## Contributing

The specification is normative. Subsystems implement their section of `docs/AMH-SPECIFICATION.md`. Domain safety (physical or otherwise) is implemented in extensions, not by expanding the kernel.

---

**Alpha Vector LLC.** Spec revision: **v9 (2026-08-21)**. See `docs/AMH-SPECIFICATION.md` for revision history.
