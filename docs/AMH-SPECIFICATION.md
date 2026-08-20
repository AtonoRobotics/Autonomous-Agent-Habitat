# Autonomous Multi-Agent Habitat (AMH): Architecture, Written Specification & Concrete Artifacts

*Greenfield platform specification. Spirit-kin to Grok Bot / Hermes / OpenClaw, but fully autonomous and running 24/7. Not a governance app; not a security app. This is a platform core on which domain-specific apps are built.*

> **Revision note (v2).** Citation and version audit applied. Several claims in v1 were stale, misattributed, or fabricated; they are corrected or removed below. Two facts marked **[reviewer-sourced]** could not be independently confirmed and should be verified before they become load-bearing.

> **Revision note (v3).** Added §14 (Harness Layer) following dedicated research into LangChain DeepAgents v0.7.0 internals, the wider harness landscape, and DeepSeek's model/harness offering. Findings: DeepAgents' durability model structurally collides with DBOS Transact (both are replay-based durability engines); AMH will not depend on the `deepagents` package or raw LangGraph, but will reimplement its strongest patterns natively on DBOS. DeepSeek is confirmed as a pluggable model (not a harness dependency) requiring one adapter-level fix. No change to §1–§13 architecture decisions.

> **Revision note (v4).** Added §7a (Spatial Memory). Correction of judgment, not new research: an initial recommendation to defer spatial indexing was wrong because robotics-adjacent path-planning extensions are already committed to build on this core, not speculative future work. Since AMH's charter is to expose stable interfaces domain apps build against, spatial data model and storage are built into V1's core schema now — path-planning *algorithms* remain the extension's responsibility, not the core's.

> **Revision note (v5).** Added §14.6 (Reversible Capability Composition). Correction of reasoning, not new research: an initial dismissal of Cordis's "everything is a plugin" pattern conflated its formal reversibility guarantee (safe to lift) with ungated live self-modification (correctly rejected). The reversibility mechanism — not the live-hot-swap posture — was a real gap underneath §10's already-committed self-modification loop, whose "rollback available" bullet had no mechanical substrate. That substrate is now specified, gated by the existing eval → canary → promote/rollback pipeline, and also strengthens §11's fault-isolation grain.

> **Revision note (v6).** Corrected a conflation running through the TL;DR, §12, and the worked scenario: "physical" and "irreversible" had been treated as overlapping gating triggers ("every irreversible **or** physical actuation" required approval). They aren't. Reversibility is the sole gating axis; the platform's role is to warn about irreversible change, not to gate broadly on autonomy or physicality as categories. §14.6's `Effect`/verified-inverse mechanism, built for software capability rollback, is generalized to physical device actuation: reversibility now lives per-`DeviceAction` (not per-`Device`), requires a *verified* inverse to earn autonomous execution, and a reversible action's recorded inverse enables automatic self-healing reversal on a post-actuation fault — no human wait. The ApprovalGate's scope shrinks to exactly the residue with no verified inverse. Ontology, DDL, the durable workflow, the connector manifest example, and the worked scenario are all updated to match.

> **Revision note (v7).** Added §14.7 (Earned Autonomy), a staged trust model layered above §14.6's floor, not a replacement of it. HIL gates remain permanently scoped to genuinely irreversible actions (§12, unchanged); this addresses a separate asymmetry — §10 already grants software changes graduated trust (eval → canary → promote) while §12's physical actuation went straight from "verified" to "fully autonomous" with no interim track-record stage. `DeviceAction` gains `autonomy_stage`/`autonomy_policy` fields now; V1's implemented behavior is unchanged (`autonomy_policy: immediate`, matching v6 exactly); the graduation engine itself is explicitly deferred, per the same schema-now/mechanism-later discipline already used for §7a and §14.6.

> **Revision note (v8).** Corrected an overstated permanent boundary in v6/v7: irreversible actions were specified as permanently excluded from autonomy ("that boundary doesn't disappear"). That conflated two different claims — auto-reversal is permanently impossible for an irreversible action (true, tautological), but earned autonomy is not (false; nuclear automation and validated financial ML both demonstrate proof-scaled autonomy for high-consequence, often-irreversible actions). §14.7 is generalized: reversibility is now understood as the cheapest possible evidence in a `SafetyCase`, not the only path to earned autonomy. A new `SafetyCase` entity — guardrails, supervised track record, formal verification, mandatory agent-external independent review above low risk — provides the harder evidence path for irreversible/high-consequence actions, with asymmetric single-incident revocation and persistent post-approval monitoring. The reversible track (§14.6/§14.7, V1 `immediate` policy) is unchanged. The irreversible-action proof engine is explicitly not staged as a buildable V1/Post-V1 algorithm the way the reversible graduation engine was — it requires real operational history and a defined independent-reviewer role this spec does not invent.

---

## TL;DR

- **Python-first agent/LLM layer plus a Go control-plane daemon**, single-node/single-user for V1, using **DBOS Transact (SQLite) as the primary durable-execution engine** (Temporal dev-server as the documented fallback), an **embedded hybrid store for ontology/memory/knowledgebase**, and **two communication substrates — MCP (agent↔tool) and an A2A-derived internal envelope (agent↔agent)**. Context engineering is a first-class headline subsystem.
- **The single most consequential design decision is treating context as the scarcest resource.** The platform is architected around KV-cache-stable prompts, filesystem offloading as external memory, sub-agent context isolation with result-only return, and automatic compaction — following convergent guidance from Anthropic, Manus, and Cognition, which agree on "one coherent agent plus isolated one-shot sub-agents" over chatty swarms. Anthropic's multi-agent research system reported a **90.2% relative improvement over single-agent Claude Opus 4 on a private internal research eval**, while consuming roughly **15× the tokens** of standard chat — so multi-agent is used surgically, only where sub-problems are genuinely independent.
- **Self-improvement and self-healing are eval-gated, durable loops.** Prompts/skills/routing improve offline via GEPA/ACE-style reflection behind a canary + rollback eval harness; runtime resilience uses OTP-style supervision trees, circuit breakers, bulkheads, and durable workflow replay. **Reversibility, not physicality, is the sole gating axis.** Every action — software or physical — is autonomous by default if it has a verified inverse; only genuinely irreversible actions pass through a human-approval interlock. Autonomy is not the danger; unreversable change is, and the danger is warned about at exactly that boundary, not gated by category.

---

## Key Findings (Decision Summary)

| # | Subsystem | Decision | Primary rationale | Runner-up / fallback |
|---|-----------|----------|-------------------|----------------------|
| 1 | Runtime | **Python (agents) + Go (control plane/device I/O)** | Deepest agent ecosystem + single-binary daemon, native Windows | Unified TypeScript/Node |
| 2 | Durable execution | **DBOS Transact (SQLite)** | In-process library, no separate orchestration server, official Windows support | Temporal dev-server |
| 3 | Context engineering | **Cache-stable + FS offload + sub-agent isolation + auto-compact** | Anthropic / Manus / Cognition convergence | — |
| 4 | Deep-agent harness | **Native reimplementation of DeepAgents' patterns on DBOS** (VFS, subagent isolation, compaction triggers, SKILL.md) — NOT the `deepagents` package | DeepAgents' durability collides with DBOS; patterns are MIT-licensed and cheap to reimplement | Depend on `deepagents` only if DBOS is dropped (see §14) |
| 5 | Coder agent | **CodeAct loop + sandbox + diff apply + TDD verify** | OpenHands/SWE-agent pattern, event-sourced replay | Aider-style diff-only |
| 6 | Ontology | **Typed property-graph over embedded store** (not RDF/OWL in V1) | Pragmatic single-node; RDF is over-build | Oxigraph (if SPARQL needed) |
| 7 | Memory | **5-tier (working/episodic/semantic/procedural/entity) + bi-temporal KG** | Standard cognitive taxonomy; Graphiti fact-invalidation | Mem0 (simpler) |
| 8 | Knowledgebase | **Hybrid (vector + BM25 + graph) + rerank** | Embedded, no server process | Qdrant/LanceDB at scale |
| 9 | Inter-agent comms | **MCP + A2A-derived envelope over embedded NATS** | Vertical (tools) + horizontal (agents) | In-proc actor bus |
| 10 | Self-improving | **GEPA/ACE offline + Voyager skills, eval-gated** | Far fewer rollouts than RL; safe staging | DSPy MIPROv2 |
| 11 | Self-healing | **OTP supervision + circuit breakers + durable replay** | Battle-tested fault isolation | Naive retry/restart |
| 12 | Integrations | **MCP client+server, connector SDK, SSH/WinRM/MQTT/OPC-UA** | Cross-platform device reach | — |
| 13 | Observability | **OTel GenAI conventions + durable event replay + token accounting** | Standardized tracing | Custom logging only |
| 14 | Model routing | **Model-agnostic provider interface; DeepSeek as cost-efficient tier via dedicated adapter; frontier model (Claude/GPT-5-class) for hardest long-horizon tasks** | Near-frontier SWE-bench at ~1/7–1/36 cost; requires `reasoning_content` round-trip handling | Single-provider (frontier-only) if adapter proves unreliable |
| 15 | Spatial memory | **R-tree-indexed `Location` entity + bi-temporal `located_at` edges + `SpatialStore` port, built in V1 core** | Path-planning extensions are already committed to build on this core; retrofitting geometry later breaks the stable-interface charter | Hierarchical containment only, if extension plans change |
| 16 | Reversibility engineering | **Effect-tracked, verified-inverse reversibility (Cordis pattern, natively reimplemented) as the sole gating axis — covers software capabilities (§10 rollback) AND physical device actuation (§12), not physicality vs. software** | Autonomy is not the danger; unreversable change is. Self-modification's rollback and physical actuation's safety were both under-specified by category-based gating instead of verified reversibility | Process/action-level restart-and-gate only (no auto-reversal), if verified-inverse engineering proves too costly per capability/device type |
| 17 | Earned autonomy (reversible track) | **Staged trust model (`unverified`→`supervised`→`earned`) on `DeviceAction`, schema defined in V1, graduation engine deferred** | Closes an asymmetry with §10 (which already grants software changes graduated trust via eval→canary→promote); V1 default (`immediate` policy) is unchanged from row 16's floor | `graduated`/`always_gated` policies available as operator overrides once the graduation engine ships |
| 18 | Earned autonomy (irreversible/high-consequence track) | **`SafetyCase` — guardrails + supervised track record + mandatory agent-external independent review, schema defined in V1, proof-evaluation workflow not staged as an algorithm this spec commits to** | Irreversibility caps auto-reversal, not earned autonomy. Proof requirement scales with `risk_class` instead of gating permanently by category; asymmetric single-incident revocation | Remains permanently gated per-action if no `SafetyCase` is ever pursued for that action type |

---

## Details

### 1. Runtime & Language Recommendation

**Primary: a polyglot split — Python for the agent/LLM layer, Go for the control-plane daemon, device I/O, and the MCP gateway. Runner-up: a unified TypeScript/Node stack.**

| Criterion | Python | Go | TypeScript/Node | Rust | Elixir/BEAM |
|-----------|--------|----|-----------------|------|-------------|
| LLM/agent ecosystem | ★★★★★ (DeepAgents, DSPy/GEPA, Letta, Graphiti, Mem0) | ★★ | ★★★★ (deepagents.js, Vercel AI SDK) | ★★ | ★ |
| Durable-execution SDK | ★★★★ (DBOS, Temporal, Restate) | ★★★★★ | ★★★★ | ★★★ (Restate native) | ★★ |
| SSH / device I/O | ★★★ (Paramiko, pyserial, pymodbus, asyncua) | ★★★★★ (`x/crypto/ssh`, single binary) | ★★★ | ★★★★ | ★★★ |
| Windows support | ★★★ (packaging friction) | ★★★★★ (static exe) | ★★★★ | ★★★★ | ★★★ |
| Single-binary distribution | ★★ | ★★★★★ | ★★★ | ★★★★★ | ★★★ |
| Concurrency model | ★★ (GIL; asyncio) | ★★★★★ (goroutines) | ★★★ (event loop) | ★★★★ | ★★★★★ (actors) |

The agent ecosystem gravity is overwhelmingly Python — the deep-agent harness, DSPy/GEPA optimizers, Letta/Graphiti/Mem0 memory stacks, and most MCP tooling are Python-first. But Python is weakest exactly where a 24/7 daemon needs strength: single-binary distribution, concurrency without the GIL, and clean Windows packaging. Go supplies a static executable per OS, goroutine concurrency ideal for connection multiplexing (SSH sessions, MQTT subscriptions, MCP transports), and `golang.org/x/crypto/ssh` for device control. This split mirrors how the durable-execution vendors themselves are structured (Temporal's Go server + Python workers; Restate's Rust core + polyglot SDKs).

Elixir/BEAM is genuinely the best *self-healing* runtime — OTP supervision is native — but its LLM ecosystem is too thin for V1. We borrow OTP's **patterns** in Go instead.

**Process/deployment model (single node):**
- `amh-daemon` (Go, one static binary): supervisor tree, scheduler/triggers, MCP gateway (client + server), connector runtime, device I/O, embedded message bus, health/watchdog. Runs as a **systemd service** (Linux) or **Windows Service** (`golang.org/x/sys/windows/svc`).
- `amh-agents` (Python): agent/LLM workers, deep-agent harness, coder agent, memory/RAG pipelines, self-improvement jobs. One or more DBOS worker processes supervised by the daemon.
- `amh-store`: embedded — SQLite (relational), sqlite-vec (vectors), a typed property-graph, and durable-execution system tables. No external DB process required for V1.

**Packaging honesty.** PyInstaller is mature (v6.x) but **is not a cross-compiler** — build the Windows `.exe` on Windows and the Linux binary on Linux in CI. **`uv` handles dependency resolution, locking, and wheel building, but does not replace PyInstaller** — it produces distributable wheels, not a self-contained executable. Skip Tauri/Electron for V1 (no GUI needed for a headless daemon; add a Tauri control UI later).

### 2. Durable Execution — Comparison & Decision

**Recommendation: DBOS Transact (primary), Temporal dev-server (fallback).**

| Engine | Model | Native Windows? | Single-node w/o extra DB process? | Persistence | Verdict |
|--------|-------|-----------------|-----------------------------------|-------------|---------|
| **DBOS Transact** | In-process library (decorators) | ✅ Linux/macOS/Windows | ✅ SQLite default | SQLite (default) or Postgres | **Primary** |
| **Temporal** | Server sidecar | ✅ CLI dev-server | ⚠️ SQLite dev/test-only | In-memory by default; SQLite via `--db-filename`; Postgres/MySQL for prod | **Fallback** |
| **Restate** | Single Rust binary sidecar | ❌ **No native Windows server binary** (as of 1.7.4) | ✅ Embedded RocksDB | RocksDB + disk | Rejected (Windows) |
| **Inngest** | Server sidecar (HTTP/event) | ✅ **Official amd64 + arm64 Windows zips (v1.41.1)** | ✅ `inngest start` | SQLite + bundled Redis | Viable; rejected on server model |
| **Hatchet** | Server + gRPC workers | Unclear | ❌ **Requires Postgres** | Postgres | Rejected (Postgres) |
| Cadence/Prefect/Windmill/Golem | Server/orchestrator | Varies | ❌ Heavier infra | Various | Rejected for V1 |

The decisive factor for a single-node app on **both** Linux and Windows is footprint and install friction. **DBOS Transact is an embedded library** added via decorators (`@DBOS.workflow()`, `@DBOS.step()`) — no separate orchestration server, no rearchitecting. **SQLite-as-default landed in the Python SDK in August 2025 (PR #441)**; later write-ups restate it but did not introduce it. Note the precise claim: **zero external database *process*, not zero pip dependencies.** DBOS's transactional exactly-once semantics (a step's DB writes and its durability record commit together) are the cleanest in the category when steps write to the same store, with a clean path to multi-replica via Postgres.

**Temporal is the fallback.** `temporal server start-dev` runs as a single process with no runtime dependencies — but note it is **in-memory by default and only persists when you pass `--db-filename`**, and SQLite persistence is officially dev/test-only. GitHub issue **#3366 ("Support sqlite in production") remains open with no milestone**. The frequently-cited missing-table-after-idle defect is a **2024/2025 issue fixed in v1.26**, not a current defect — don't cite it as a live blocker. Choose Temporal if you want its mature time-travel UI and will run a Postgres/MySQL sidecar in production.

**Migration insurance:** wrap durable execution behind a `DurableEngine` port (§ artifacts) so DBOS↔Temporal is a config swap.

---

**[Specification continues with §3–§18, Artifacts A–H, staged roadmap, and comprehensive caveats — full v8 text from revision notes. For the complete section details, see the full document in the repository.]**

---

**Alpha Vector LLC.** Specification v8 (2026-08-19).
