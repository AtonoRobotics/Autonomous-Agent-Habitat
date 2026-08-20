# Autonomous Multi-Agent Habitat (AMH)

A platform for autonomous agents doing real work under bounded authority. Fully autonomous, running 24/7, not a governance app or orchestration layer, but the core on which domain-specific applications are built.

**Designed for:** AI agents that control physical devices (via SSH, OPC-UA, MQTT, WinRM), learn from operational history, and self-heal on failure — with reversibility as the sole gating axis. Irreversible or high-consequence actions require evidence-backed approval, not blanket denial.

**Architecture (v8):** Python agent/LLM layer (DeepAgents patterns reimplemented natively on DBOS) + Go control-plane daemon (systemd/Windows Service), DBOS Transact on SQLite for durable-execution, embedded hybrid KG + vector store, spatial memory R-tree index, reversible capability composition, effect-tracked self-modification, and earned autonomy staged by evidence.

## Layout

```
daemon/           Go control plane (single binary per OS)
├── cmd/amh-daemon        systemd/Windows-service entry
├── supervisor/           OTP-style supervision tree
├── scheduler/            cron/triggers/event sources
├── bus/                  embedded NATS + blackboard
├── mcp/                  MCP client + server (stdio, Streamable HTTP)
├── connectors/           REST/GraphQL/webhook + SSH/WinRM/MQTT/OPC-UA
├── interlocks/           ApprovalGate enforcement
└── health/               watchdog, circuit breakers, bulkheads

agents/           Python agent/LLM layer (uv-managed)
├── harness/              deep-agent: planning, VFS, subagents, middleware
├── coder/                CodeAct sandbox loop
├── context/              budget mgr, compactor, offload
├── memory/               5-tier + bi-temporal KG
├── kb/                   hybrid RAG (sqlite-vec + FTS5 + graph)
├── selfimprove/          GEPA/ACE/Voyager + eval-gate
└── workflows/            DBOS durable workflows & activities

contracts/        SHARED public interfaces
├── ontology.schema.json
├── envelope.schema.json
├── manifests/
└── proto/                daemon↔agent gRPC

sdk/              domain-app extension SDK (stable public API)
├── python/               amh_sdk (Protocols)
└── ts/                   @amh/sdk (interfaces)

store/            DDL, migrations
migrations/       ordered SQL migrations for the business ledger
evals/            eval suites, canary configs
deploy/           systemd unit, Windows service, installers

clients/          field surfaces (Linux page, iOS app)
fixtures/         test-only packs and throwaway keys
docker/           tenant-computer and local-stack images
scripts/          bootstrap, pack signing, local run
state/            per-tenant on-disk state (git-ignored)
```

## Getting started

**Read first:** `docs/AMH-SPECIFICATION.md` — the complete architecture spec, design rationale, component decisions, and staged V0/V1 roadmap.

**V0 (walking skeleton):** Go daemon supervisor + Python agent layer on DBOS/SQLite, one SSH connector, one reversible device action, approval gate for irreversible actions, MCP client, context budget manager, OTel tracing. Greenhouse scenario (monitor temp, open vent on threshold, self-heal on fault) runs end-to-end and survives daemon restart.

**V1 (autonomous habitat):** orchestrator/worker multi-agent (3–5 sub-agents for independent work), full 5-tier memory + bi-temporal KG, coder agent + sandbox, self-improvement (GEPA/ACE/Voyager behind eval/canary/promote), full self-healing (capability-level granularity), earned autonomy graduation engine (reversible track), Connector SDK, MCP server, extension/plugin model.

## Key architectural decisions

| # | Subsystem | Decision | Why |
|-|-|-|-|
| 1 | Runtime | Python + Go | Deepest agent ecosystem + single-binary daemon, native Windows |
| 2 | Durable execution | DBOS Transact (SQLite) | In-process library, no separate server, official Windows support |
| 3 | Gating axis | Reversibility, not physicality | Autonomy is not the danger; unreversable change is |
| 4 | Harness | Native DeepAgents reimplementation | Patterns are MIT-licensed; package dependency collides with DBOS durability |
| 5 | Context | KV-cache-stable + FS offload + sub-agent isolation | Anthropic/Manus/Cognition convergence; 15× token cost for multi-agent justified only for independent work |
| 6 | Memory | 5-tier CoALA + bi-temporal KG | Standard cognitive taxonomy; Graphiti fact-invalidation model |
| 7 | Spatial | R-tree-indexed Location + bi-temporal located_at edges | Path-planning extensions already committed; retrofitting later breaks stability |
| 8 | Reversibility | Effect-tracked, verified-inverse reversibility | Covers software capabilities (rollback) AND physical device actuation (auto-reversal) |
| 9 | Earned autonomy | Staged trust (reversible track) + SafetyCase (irreversible track) | Reversible actions automate readily; irreversible actions earn autonomy via evidence |
| 10 | Self-mod safety | Independent tamper-proof instrumentation | Prevent reward-hacking; evals run on separate tooling the agent cannot edit |

## Permissions & safety gates

- **Reversible actions** (verified inverse exists): autonomous by default. Self-healing supervisor auto-reverses on post-actuation fault, no human wait.
- **Irreversible actions** (no verified inverse, no approved SafetyCase): ApprovalGate required. Rate-limited and logged.
- **High-consequence actions** (SafetyCase track): earn autonomy via independent review, guardrails proof, supervised track record — staged per-`risk_class`, not blanket excluded.

## Contributing

The specification is normative. Subsystems implement their §N section of `docs/AMH-SPECIFICATION.md` — each carrying its own rules and refusals (see §A, monorepo layout, and the per-subsystem READMEs).

For encoding patterns, stability contracts, and extension points, consult the Artifact sections: B (Protocol ports), C (Ontology), D (Inter-agent envelope), E (DDL), F (durable workflows), G (manifests), H (worked scenario).

---

**Alpha Vector LLC.** Spec revision: v8 (2026-08-19). See `docs/AMH-SPECIFICATION.md` for revision history.
