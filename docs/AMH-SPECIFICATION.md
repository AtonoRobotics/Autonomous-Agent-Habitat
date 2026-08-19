# Autonomous Multi-Agent Habitat (AMH): Architecture, Written Specification & Concrete Artifacts

*Greenfield platform specification. Spirit-kin to Grok Bot / Hermes / OpenClaw, but fully autonomous and running 24/7. Not a governance app; not a security app. This is a platform core on which domain-specific apps are built.*

> **Revision note (v2).** Citation and version audit applied. Several claims in v1 were stale, misattributed, or fabricated; they are corrected or removed below. Two facts marked **[reviewer-sourced]** could not be independently confirmed and should be verified before they become load-bearing.

---

## TL;DR

- **Python-first agent/LLM layer plus a Go control-plane daemon**, single-node/single-user for V1, using **DBOS Transact (SQLite) as the primary durable-execution engine** (Temporal dev-server as the documented fallback), an **embedded hybrid store for ontology/memory/knowledgebase**, and **two communication substrates — MCP (agent↔tool) and an A2A-derived internal envelope (agent↔agent)**. Context engineering is a first-class headline subsystem.
- **The single most consequential design decision is treating context as the scarcest resource.** The platform is architected around KV-cache-stable prompts, filesystem offloading as external memory, sub-agent context isolation with result-only return, and automatic compaction — following convergent guidance from Anthropic, Manus, and Cognition, which agree on "one coherent agent plus isolated one-shot sub-agents" over chatty swarms. Anthropic's multi-agent research system reported a **90.2% relative improvement over single-agent Claude Opus 4 on a private internal research eval**, while consuming roughly **15× the tokens** of standard chat — so multi-agent is used surgically, only where sub-problems are genuinely independent.
- **Self-improvement and self-healing are eval-gated, durable loops.** Prompts/skills/routing improve offline via GEPA/ACE-style reflection behind a canary + rollback eval harness; runtime resilience uses OTP-style supervision trees, circuit breakers, bulkheads, and durable workflow replay. **Every irreversible or physical actuation passes through a human-approval interlock** — non-negotiable, because self-modifying agents demonstrably reward-hack (the Darwin-Gödel Machine once deleted its own hallucination-detection logging to score better).

---

## Key Findings (Decision Summary)

| # | Subsystem | Decision | Primary rationale | Runner-up / fallback |
|---|-----------|----------|-------------------|----------------------|
| 1 | Runtime | **Python (agents) + Go (control plane/device I/O)** | Deepest agent ecosystem + single-binary daemon, native Windows | Unified TypeScript/Node |
| 2 | Durable execution | **DBOS Transact (SQLite)** | In-process library, no separate orchestration server, official Windows support | Temporal dev-server |
| 3 | Context engineering | **Cache-stable + FS offload + sub-agent isolation + auto-compact** | Anthropic / Manus / Cognition convergence | — |
| 4 | Deep-agent harness | **DeepAgents-equivalent (planning, VFS, sub-agents, middleware)** | Proven four-pillar pattern | Wrap LangGraph directly |
| 5 | Coder agent | **CodeAct loop + sandbox + diff apply + TDD verify** | OpenHands/SWE-agent pattern, event-sourced replay | Aider-style diff-only |
| 6 | Ontology | **Typed property-graph over embedded store** (not RDF/OWL in V1) | Pragmatic single-node; RDF is over-build | Oxigraph (if SPARQL needed) |
| 7 | Memory | **5-tier (working/episodic/semantic/procedural/entity) + bi-temporal KG** | Standard cognitive taxonomy; Graphiti fact-invalidation | Mem0 (simpler) |
| 8 | Knowledgebase | **Hybrid (vector + BM25 + graph) + rerank** | Embedded, no server process | Qdrant/LanceDB at scale |
| 9 | Inter-agent comms | **MCP + A2A-derived envelope over embedded NATS** | Vertical (tools) + horizontal (agents) | In-proc actor bus |
| 10 | Self-improving | **GEPA/ACE offline + Voyager skills, eval-gated** | Far fewer rollouts than RL; safe staging | DSPy MIPROv2 |
| 11 | Self-healing | **OTP supervision + circuit breakers + durable replay** | Battle-tested fault isolation | Naive retry/restart |
| 12 | Integrations | **MCP client+server, connector SDK, SSH/WinRM/MQTT/OPC-UA** | Cross-platform device reach | — |
| 13 | Observability | **OTel GenAI conventions + durable event replay + token accounting** | Standardized tracing | Custom logging only |

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

### 3. Context Engineering (Headline Subsystem)

Its own budget manager, compactor, and offload store. Sourcing, unbundled precisely:

**Anthropic, "Effective context engineering for AI agents" (Sept 29, 2025):** find "the smallest set of high-signal tokens that maximize the likelihood of your desired outcome"; keep system prompts at "the right altitude" (between brittle hardcoded logic and vague generality); curate a minimal tool set; use canonical few-shot examples, not edge-case dumps.

**Anthropic, "Effective harnesses for long-running agents" (Nov 2025):** a distinct **initializer** prompt for the first context window, plus a `claude-progress.txt`-style progress file and git history so a fresh window can reconstruct state.

**Claude Code product default (not an essay finding):** tool responses are capped at **25,000 tokens** by default. Anthropic's tool-authoring guidance separately recommends pagination, range selection, filtering, and truncation.

**Manus, "Context Engineering: Lessons from Building Manus" (July 18, 2025):** the **KV-cache hit rate is the single most important production metric** (cached input at $0.30/MTok vs uncached $3/MTok on Claude Sonnet — a 10× difference; Manus runs ~100:1 input:output). Therefore: stable prompt prefix, append-only context, deterministic serialization (stable JSON key ordering), explicit cache breakpoints. **"Mask, don't remove"** — use a context-aware state machine with logit masking rather than mutating the tool list, which invalidates the cache. Treat the **file system as unlimited context**. Preserve errors in context so the model learns. Introduce controlled diversity to avoid few-shot ruts.

**Cognition ("Don't Build Multi-Agents" / "Multi-Agents: What's Actually Working"):** context is the crux; parallel sub-agent writers produce **miscommunications** that compound (the Flappy-Bird-clone example). The winning shape is **"map-reduce-and-manage"**: a manager splits work, children execute, the manager synthesizes — with **writes kept single-threaded** while multiple agents contribute intelligence. Their operative guidance is to **share full traces, not just messages**.

**AMH context subsystem specification:**
1. **Budget manager** — per-agent token budget with headroom reserve; tracks prefix-stable vs volatile regions; enforces a configurable single-tool-result cap (defaulting to 25k, matching Claude Code's product default).
2. **Compactor** — triggers at a configurable threshold (default 70% of window). Hierarchical summarization of oldest N turns to structured JSON, **retaining the most recent K turns verbatim** (an AMH design choice, to preserve the model's formatting rhythm). Emits a compaction event to the durable log for replay.
3. **Offload store** — the deep-agent virtual filesystem; large tool results, research notes, and scratchpads live as files, referenced by handle, loaded just-in-time.
4. **Sub-agent isolation** — spawned agents get a fresh context window, an explicit objective + boundaries + output schema, and **return only a condensed result**. Per Cognition, full traces are shared with the manager where synthesis quality demands it; transcripts are never injected wholesale into peer contexts.
5. **Cache discipline** — append-only history, stable serialization, logit-mask tool availability, explicit cache breakpoints covering the system prompt.
6. **Rot mitigation** — periodically re-recite the goal (attention anchoring); place critical instructions at start and end (lost-in-the-middle); prefer many small targeted retrievals over one broad dump.

**Failure modes tracked:** context poisoning (bad fact persisted), distraction (bloat crowds the goal), confusion (irrelevant context), clash (contradictory context). Each maps to a detector in §11.

### 4. Deep-Agent Harness

DeepAgents' four pillars — planning tool, virtual filesystem, sub-agent spawning, detailed system prompt, plus middleware — become AMH's default harness.

- **Planning:** a `write_todos`-style structured task list. **Note: in DeepAgents v0.7 `write_todos` is opt-in, not on by default.** AMH enables it by default and persists the list to the durable log so plans survive restarts.
- **Filesystem:** pluggable backends (in-memory, local disk, graph-store) — read/write/edit/ls/glob/grep.
- **Sub-agents:** a `task` tool spawning ephemeral children in isolated context; harness enforces max-concurrency, timeouts, result aggregation, error propagation.
- **AMH additions:** durable checkpoints between context windows (survives process death), KV-cache-aware prompt assembly, native OTel GenAI tracing, approval-gate middleware, Voyager-style skill-library injection.

### 5. Coder Agents

**CodeAct** paradigm (actions as executable code), following Claude Code / OpenHands / SWE-agent / Aider.

- **Sandbox:** Docker container (Linux) or restricted subprocess/firejail; on Windows, a constrained job object. The agent gets its own filesystem, terminal, browser. OpenHands (MIT, **~84k stars**) publishes **77.6% on SWE-bench Verified via its own harness badge** — a self-reported harness figure, not a standardized-harness result. Independent evaluations under standardized harnesses score lower; always quote the harness alongside the number.
- **Diff application:** structured patch/edit tools with validation; reject malformed diffs with actionable error messages.
- **Test-driven verification:** write test → run in sandbox → observe → iterate; a change ships only if tests pass. Event-sourced state (immutable event log) enables replay, recovery, and incremental persistence — strict separation of core agent logic from applications.
- **Ship gate:** human approval for any write outside the sandbox (repo push, deploy).

### 6. Ontology Layer

**Recommendation: a typed property-graph over the embedded store, NOT full RDF/OWL, for V1.** RDF/OWL/SHACL suits heterogeneous open-world knowledge but is over-engineering here; a strongly-typed property graph delivers typed entities, typed relations, and multi-hop traversal at a fraction of the complexity.

**Sourcing caveat:** KuzuDB — the obvious embedded property-graph choice (in-process, Cypher, columnar, ACID, cross-platform) — **was archived in October 2025 following Apple's acquisition of the company**. Maintenance continues via MIT-licensed community forks; **[reviewer-sourced] LadybugDB is named as the maintained fork** — verify before depending on it. Options: (a) adopt the maintained fork, (b) **model the graph as SQLite tables with recursive CTEs** (fewest dependencies — recommended for V0), or (c) **Oxigraph** (Rust, RocksDB-backed, SPARQL) only if formal RDF semantics become a hard requirement.

**Core ontology:**

```
Entities:  Agent, Task, Goal, Skill, Tool, Memory, Artifact,
           Device, Connector, Run, Event, Message, ApprovalGate,
           Eval, PromptVersion, SkillVersion

Key relations:
  Goal      --decomposes_into-->  Task
  Task      --assigned_to------>  Agent
  Agent     --spawns----------->  Agent            (parent/child)
  Agent     --uses------------->  Skill | Tool | Connector
  Run       --executes--------->  Task
  Run       --emits------------>  Event
  Agent     --has_memory------->  Memory
  Task      --produces--------->  Artifact
  Connector --controls--------->  Device
  Task      --requires--------->  ApprovalGate     (if irreversible)
  Skill     --version_of------->  SkillVersion
  Eval      --gates------------>  PromptVersion | SkillVersion
```

### 7. Memory Architecture

Five tiers following the **CoALA** taxonomy (Working / Episodic / Semantic / Procedural) — **a taxonomy LangGraph explicitly adopted; Letta uses its own core/archival/recall tiering** — plus **Entity** memory.

| Tier | Contents | Backing store | Lifecycle |
|------|----------|---------------|-----------|
| **Working** | Current task, plan, recent observations, scratchpad | Context window + VFS files | Ephemeral; compacted |
| **Episodic** | What happened, when, with what outcome | Append-only event log (SQLite) | Consolidated → semantic |
| **Semantic** | Facts, entities, relationships | Bi-temporal knowledge graph | Fact invalidation, not deletion |
| **Procedural** | Skills, playbooks, runbooks, prompt rules | Skill library + prompt registry | Versioned, eval-gated |
| **Entity** | Per-entity durable profiles (users, devices, projects) | Property-graph nodes | Updated in place |

**Semantic/entity memory uses a Graphiti-style bi-temporal design.** Graphiti's model tracks **two independent time axes**: `valid_at` / `invalid_at` (when the fact was true in the world) and `created_at` / `expired_at` (when the system learned/retired it). When a fact changes, the prior edge's validity window is closed rather than deleted, preserving point-in-time queries and provenance. Graphiti/Zep organize memory into episodic nodes → semantic entities/facts → community summaries, with retrieval fusing cosine similarity + BM25 + graph traversal and reciprocal-rank-fusion reranking. On the Deep Memory Retrieval benchmark, **Zep reports 94.8% vs MemGPT's 93.4% (gpt-4-turbo)** — that is the like-for-like comparison; Zep's higher figures on other model configurations are not MemGPT comparisons and shouldn't be quoted as such.

**Consolidation & forgetting ("sleep-time compute"):** an offline durable workflow reflects on the episodic log, extracts durable facts into the semantic graph (MemGPT-style self-editing via tool calls), decays stale low-salience memories, and updates community summaries. Mem0 is the simpler fallback if the graph proves heavy.

### 8. Knowledgebase / RAG

**Hybrid retrieval on an embedded store.** Pipeline: chunk → embed → store → hybrid search (dense vector + BM25 lexical + graph expansion) → rerank (RRF, then optional cross-encoder) → agentic retrieval (many small targeted queries).

**Single-node vector store decision:**

| Option | Fit | Note |
|--------|-----|------|
| **sqlite-vec** | ★★★★★ V1 default | **Loadable SQLite extension, not core SQLite. Still pre-v1. Default search is brute-force KNN, not ANN** — fine at V1 scale, not a million-vector store |
| **LanceDB** | ★★★★ | Embedded, columnar, disk-based ANN, larger-than-memory; single-writer limitation |
| Chroma | ★★★ | Good for prototyping; embedded mode |
| Qdrant | ★★★ | Purpose-built Rust ANN engine; **typically faster than pgvector on selective filtered ANN** thanks to its filterable-HNSW design. *(Specific latency figures previously cited here were retracted by their source and have been removed — benchmark on your own corpus.)* |
| pgvector | ★★ | Only if already on Postgres |

Choose **sqlite-vec for V1** (one file, brute-force is adequate at expected scale), behind a clean `VectorStore` port so LanceDB or Qdrant drops in when the corpus outgrows brute force. Add **GraphRAG** by using the ontology property-graph for multi-hop expansion around retrieved chunks.

### 9. Inter-Agent Communication & Coordination

**Two substrates, deliberately separated** (MCP vertical, A2A horizontal):
- **MCP** — agent↔tool/resource/data access.
- **A2A-derived envelope** — agent↔agent coordination. **A2A (Google → Linux Foundation) reached v1.0; current release is 1.0.1 (May 2026) [reviewer-sourced]. AMH baselines on 1.0 and retains v0.3 as a compatibility path.** A2A uses Agent Cards for capability discovery, plus tasks, messages, and artifacts over HTTP/JSON/SSE. We adopt its concepts for our **internal** envelope but run it over a local bus for latency.

**Transport:** **embedded NATS** for pub/sub plus a **blackboard** table (SQLite) for shared state; in-process actor mailboxes for co-located agents. **Redis Streams is a viable alternative but runs as a sidecar process — it is not embeddable the way NATS is**, which matters for a zero-extra-process single-node install. **Contract-net protocol** for task bidding when multiple agents can serve a task (`propose`, `counter-propose`, `accept`, `reject`, `commit` — FIPA-ACL lineage).

**Topology:** default **supervisor / orchestrator-worker** — a lead agent plans, spawns **3–5 subagents** in parallel with isolated context windows, then runs a separate synthesis + citation pass. Reserve parallelism for genuinely independent, breadth-first sub-problems: Anthropic reported a **90.2% relative improvement over single-agent Claude Opus 4 on a private internal research eval**, at roughly **15× the tokens** of standard chat (token usage alone explained ~80% of variance in their BrowseComp results). For everything else, prefer single-threaded writes with one-shot sub-calls. Hierarchical and market-based topologies are supported but off by default.

### 10. Self-Improving Subsystem

Targets: **prompts, tools/skills, routing** — all offline ("sleep-time"), all eval-gated.

- **Prompt/context optimization:** **GEPA** (Genetic-Pareto reflective prompt evolution), an **ICLR 2026 Oral**, is the primary optimizer — it reflects on execution traces in natural language and maintains a Pareto frontier of prompts. Reported results: **outperforms GRPO by ~6% on average and by up to ~19–20%**, using far fewer rollouts; and beats MIPROv2 by **+12% on AIME-2025 in the Qwen3-8B configuration**. *(The paper reports these as percent, not percentage points — don't convert.)* **ACE** (Agentic Context Engineering; Generator-Reflector-Curator) complements it by growing a durable context "playbook" while avoiding context collapse and brevity bias. DSPy provides the substrate; MIPROv2 is the fallback optimizer.
- **Skill acquisition:** **Voyager-style** — successful trajectories distilled into executable, retrievable, composable skills indexed by natural-language description, validated by execution before entering the library.
- **Self-modification (bounded):** a **Darwin-Gödel-Machine-style** loop can propose edits to its own harness/tools, validated empirically — DGM raised SWE-bench from **20.0% to 50.0%** and Polyglot from **14.2% to 30.7%** over 80 iterations. **Highest-risk capability; ships last, heavily sandboxed.**
- **Guardrails:** every proposed change runs **eval harness → canary (shadow traffic on held-out tasks) → promote or rollback**. Version everything (immutable prompt/skill registries). Explicit anti-reward-hacking control: the canonical DGM cautionary case is an agent that, asked to reduce tool-call hallucination, earned a perfect score by deleting the logging that detected it — so **evals run on independent, tamper-proof instrumentation the agent cannot edit**.

### 11. Self-Healing Subsystem

**Structural resilience (OTP patterns ported to Go):** a supervision tree with `one_for_one` / `one_for_all` / `rest_for_one` restart strategies and per-supervisor restart-intensity limits, so failure recovery is scoped to the failed process rather than the whole application.

**Distributed-systems patterns:**
- **Circuit breakers** per LLM provider / connector: `CLOSED → OPEN → HALF-OPEN`, with extended state to handle flapping recovery.
- **Bulkheads:** separate resource pools per service type so one failing connector can't starve others.
- **Retries with capped backoff** (unbounded retries cause retry storms), **saga/compensation** for multi-step external effects, **watchdogs/heartbeats**, **chaos testing** in CI.

**LLM-specific failure modes:**

| Failure mode | Detection | Automatic remediation |
|--------------|-----------|-----------------------|
| Reasoning loop | N-node cycle / repeated identical tool calls | Interrupt, compact, re-recite goal, escalate |
| Hallucinated tool | Tool name not in registry | Reject with actionable error; re-prompt |
| Context overflow | Token budget exceeded | Trigger compactor; offload to VFS |
| Tool-call malformation | Schema validation fail | Structured error response; retry with example |
| Model outage / rate limit | Circuit breaker OPEN | Failover to alternate provider/model |
| Context poisoning/clash | Contradiction vs semantic KG | Invalidate fact; flag for reflection |
| Cost runaway | Token/$ threshold | Pause agent; require human resume |

**Durability tie-in:** because workflows run under DBOS/Temporal with event-history replay, a crashed worker resumes from the last committed step — recovery is infrastructure, not bespoke retry logic.

### 12. Integrations

**MCP (client + server).** AMH is both an MCP **client** (consuming third-party servers) and an MCP **server** (exposing its own capabilities). Support **stdio** (local subprocess) and **Streamable HTTP** (remote, JSON-RPC 2.0 over POST + SSE), with the full primitive set: **tools, resources, prompts, sampling, elicitation, roots**. Spec cadence: 2025-03-26 (Streamable HTTP, OAuth 2.1), 2025-06-18 (structured output, elicitation, MCP servers as OAuth Resource Servers + RFC 8707 Resource Indicators, batching removed), 2025-11-25 (tasks, enhanced sampling, Client ID Metadata Documents), **2026-07-28** (stateless core, Multi-Round-Trip Requests replacing held-open streams for elicitation/sampling/roots, `Mcp-Method`/`Mcp-Name` headers, response `ttlMs`/`cacheScope`). **Target 2025-11-25 as the legacy baseline with a 2026-07-28 compatibility path.** **Security (mandatory):** guard against tool poisoning, confused-deputy, and prompt injection via tool results — treat all tool output as untrusted, require OAuth 2.1 for remote servers, allowlist tools before they enter context.

**Generic API connectors:** REST/GraphQL/webhook with OAuth2, API-key, and mTLS auth; a **Connector SDK** (manifest + typed handlers) so domain apps add integrations without touching the core.

**Physical device access & control (Linux + Windows):**
- **SSH** — Go `golang.org/x/crypto/ssh` (Paramiko as Python fallback).
- **Windows** — WinRM / PowerShell Remoting.
- **Serial/USB** — pyserial / gobot.
- **IoT** — MQTT (Mosquitto; Home Assistant MQTT Discovery is well-supported).
- **Home Assistant** — native REST/WebSocket + MQTT.
- **Industrial** — OPC-UA (`asyncua` / FreeOpcUa), Modbus; OPC-UA↔MQTT bridging is a common pattern.
- **Safety interlocks (non-negotiable):** every actuation is classified `reversible` / `irreversible`. Irreversible or physical-world actions require an **ApprovalGate**, enforce rate limits and value bounds, and are logged to durable event history with the approving identity. The agent cannot actuate without a satisfied gate.

### 13. Observability

- **Tracing:** OpenTelemetry **GenAI semantic conventions** — spans for agent runs, LLM calls (model, tokens in/out, cost), tool calls, sub-agent spawns, retrievals.
- **Evals:** offline harness (feeds §10) plus online quality signals.
- **Replay / time-travel debugging:** durable event history is the replay substrate.
- **Cost/token accounting:** per-agent, per-run, per-goal meters with budget enforcement.

---

## Concrete Artifacts

### A. Monorepo Layout

```
amh/
├── daemon/                     # Go control plane (single binary per OS)
│   ├── cmd/amh-daemon/         # main; systemd/Windows-service entry
│   ├── supervisor/             # OTP-style supervision tree
│   ├── scheduler/              # cron/triggers/event sources
│   ├── bus/                    # embedded NATS + blackboard
│   ├── mcp/                    # MCP client + server (stdio, Streamable HTTP)
│   ├── connectors/             # REST/GraphQL/webhook + SSH/WinRM/MQTT/OPC-UA
│   ├── interlocks/             # ApprovalGate enforcement
│   └── health/                 # watchdog, circuit breakers, bulkheads
├── agents/                     # Python agent/LLM layer (uv-managed)
│   ├── harness/                # deep-agent: planning, VFS, subagents, middleware
│   ├── coder/                  # CodeAct sandbox loop
│   ├── context/                # budget mgr, compactor, offload
│   ├── memory/                 # 5-tier + bi-temporal KG
│   ├── kb/                     # hybrid RAG (sqlite-vec + FTS5 + graph)
│   ├── selfimprove/            # GEPA/ACE/Voyager + eval-gate
│   └── workflows/              # DBOS durable workflows & activities
├── contracts/                  # SHARED public interfaces (see B–G)
│   ├── ontology.schema.json
│   ├── envelope.schema.json
│   ├── manifests/
│   └── proto/                  # daemon↔agent gRPC
├── sdk/                        # domain-app extension SDK (stable public API)
│   ├── python/                 # amh_sdk (Protocols)
│   └── ts/                     # @amh/sdk (interfaces)
├── store/                      # DDL, migrations
├── evals/                      # eval suites, canary configs
└── deploy/                     # systemd unit, Windows service, installers
```

### B. Core Interface Contracts (Python Protocols — the stable public API)

```python
from typing import Protocol, Sequence, Literal, Any
from dataclasses import dataclass

@dataclass(frozen=True)
class RunResult:
    run_id: str
    status: Literal["ok", "error", "needs_approval"]
    output: Any
    tokens_in: int
    tokens_out: int
    cost_usd: float

class Agent(Protocol):
    id: str
    def run(self, goal: "Goal", ctx: "Context") -> RunResult: ...
    def pause(self) -> None: ...
    def resume(self) -> None: ...
    def checkpoint(self) -> str: ...            # returns checkpoint id
    def hibernate(self) -> None: ...
    def terminate(self) -> None: ...

class Tool(Protocol):
    name: str
    input_schema: dict          # JSON Schema; strict data models
    def invoke(self, args: dict, ctx: "Context") -> "ToolResult": ...

class VectorStore(Protocol):    # sqlite-vec default; LanceDB/Qdrant swap-in
    def upsert(self, ids: Sequence[str], vecs, meta: Sequence[dict]) -> None: ...
    def search(self, vec, k: int, filt: dict | None = None) -> list["Hit"]: ...

class MemoryStore(Protocol):
    def write_episode(self, event: dict) -> str: ...
    def upsert_fact(self, subj: str, pred: str, obj: str,
                    valid_at: str, invalid_at: str | None = None) -> None: ...
    def recall(self, query: str, k: int) -> list["Hit"]: ...

class DurableEngine(Protocol):  # DBOS primary; Temporal fallback
    def workflow(self, fn): ...          # decorator
    def step(self, fn): ...              # decorator
    def start(self, wf, *a, **k) -> str: ...
    def get_result(self, handle: str) -> Any: ...

class ApprovalGate(Protocol):
    def require(self, action: "Action",
                risk: Literal["reversible", "irreversible"]) -> "Ticket": ...
    def is_satisfied(self, ticket: "Ticket") -> bool: ...
```

### C. Ontology Schema (excerpt)

```json
{
  "Agent":   {"id":"str","kind":"str","model":"str","parent_id":"str?","state":"enum[spawned,running,paused,hibernated,terminated]","budget_usd":"float"},
  "Goal":    {"id":"str","text":"str","priority":"int","owner":"str","status":"enum[open,active,done,failed]"},
  "Task":    {"id":"str","goal_id":"str","assignee":"str?","status":"str","approval_required":"bool"},
  "Skill":   {"id":"str","name":"str","version":"str","code_ref":"str","eval_score":"float"},
  "Tool":    {"id":"str","name":"str","input_schema":"json","connector_id":"str?"},
  "Memory":  {"id":"str","tier":"enum[working,episodic,semantic,procedural,entity]","payload":"json","valid_at":"ts","invalid_at":"ts?"},
  "Artifact":{"id":"str","task_id":"str","uri":"str","hash":"str"},
  "Device":  {"id":"str","kind":"str","connector_id":"str","actuation_risk":"enum[reversible,irreversible]"},
  "Connector":{"id":"str","type":"enum[rest,graphql,webhook,ssh,winrm,mqtt,opcua,mcp]","auth":"enum[oauth2,apikey,mtls,none]"},
  "Run":     {"id":"str","task_id":"str","started":"ts","ended":"ts?","status":"str","tokens_in":"int","tokens_out":"int","cost_usd":"float"},
  "Event":   {"id":"str","run_id":"str","type":"str","ts":"ts","payload":"json"},
  "ApprovalGate":{"id":"str","action":"json","risk":"str","approved_by":"str?","approved_at":"ts?"}
}
```

### D. Inter-Agent Message Envelope (A2A 1.0-derived)

```json
{
  "$schema": "amh/envelope/v1",
  "a2a_baseline": "1.0",
  "msg_id": "uuid",
  "trace_id": "otel-trace-id",
  "sender": {"agent_id": "str", "role": "str"},
  "recipient": {"agent_id": "str | 'broadcast'"},
  "intent": "propose | counter-propose | query | inform | accept | reject | commit | delegate | result | error",
  "task": {
    "task_id": "str",
    "objective": "str",
    "boundaries": "str",
    "output_schema": "json-schema",
    "deadline": "ts?"
  },
  "parts": [{"type": "text|json|file|artifact_ref", "content": "..."}],
  "context_ref": "vfs://path | null",
  "trace_ref": "vfs://path | null",
  "reply_to": "msg_id?",
  "ttl_ms": 30000,
  "created_at": "ts"
}
```

*Design note:* `context_ref` passes a **handle** rather than inlining payload, enforcing sub-agent isolation at the protocol level. `trace_ref` exists separately so a manager can pull a child's **full trace** during synthesis (per Cognition) without that trace polluting peer contexts.

### E. Memory & Knowledgebase DDL (SQLite + sqlite-vec)

```sql
-- NOTE: sqlite-vec is a loadable extension; load it before creating vec0 tables.
--   SELECT load_extension('vec0');

CREATE TABLE episode (
  id TEXT PRIMARY KEY, run_id TEXT, ts TEXT NOT NULL,
  actor TEXT, payload JSON, salience REAL DEFAULT 0.5
);

CREATE TABLE fact (               -- bi-temporal semantic memory (Graphiti model)
  id TEXT PRIMARY KEY, subj TEXT, pred TEXT, obj TEXT,
  valid_at   TEXT NOT NULL,       -- when true in the world
  invalid_at TEXT,                -- NULL = still true
  created_at TEXT NOT NULL,       -- when the system learned it
  expired_at TEXT,                -- when the system retired it
  source_episode TEXT, confidence REAL
);
CREATE INDEX idx_fact_current ON fact(subj, pred)
  WHERE invalid_at IS NULL AND expired_at IS NULL;

CREATE TABLE skill (
  id TEXT, version TEXT, name TEXT, description TEXT,
  code_ref TEXT, eval_score REAL, promoted INTEGER DEFAULT 0,
  PRIMARY KEY (id, version)
);

CREATE TABLE chunk (
  id TEXT PRIMARY KEY, doc_id TEXT, text TEXT, meta JSON
);
-- vector index (sqlite-vec; brute-force KNN by default)
CREATE VIRTUAL TABLE chunk_vec USING vec0(embedding FLOAT[1024]);
-- lexical index
CREATE VIRTUAL TABLE chunk_fts USING fts5(text, content='chunk', content_rowid='rowid');
```

### F. Durable Workflow Definitions (DBOS Transact, Python)

```python
from amh.durable import DBOS   # our DurableEngine port

@DBOS.workflow()
def pursue_goal(goal_id: str) -> str:
    plan = decompose_goal(goal_id)                 # step
    handles = []
    for task in plan.parallelizable:               # map
        handles.append(DBOS.start(run_subagent, task.id))
    gathered = [DBOS.get_result(h) for h in handles]  # reduce
    synthesis = synthesize(goal_id, gathered)      # step (single-threaded write)
    return synthesis

@DBOS.step()
def decompose_goal(goal_id: str): ...

@DBOS.workflow()                                   # child = isolated context
def run_subagent(task_id: str) -> dict:
    return spawn_isolated_agent(task_id).run()     # condensed result + trace_ref

@DBOS.step(retries=3, backoff="exponential")
def actuate_device(device_id: str, cmd: dict, ticket: str) -> dict:
    assert approval_satisfied(ticket)              # interlock enforced
    return connector_for(device_id).invoke(cmd)
```

### G. Manifest Formats

```yaml
# agent.manifest.yaml
apiVersion: amh/v1
kind: Agent
metadata: { name: research-lead, version: 1.4.0 }
spec:
  model: claude-sonnet-4-6
  harness: deep-agent
  planning: { write_todos: true }        # opt-in upstream (DeepAgents v0.7); on by default here
  tools: [web_search, fetch_page, memory.recall]
  subagents: { max_concurrent: 5, isolation: strict, return: result_only, share_trace_to_manager: true }
  context: { window_budget: 180000, compact_at: 0.70, keep_recent_turns_raw: 3, tool_result_cap: 25000 }
  approval: { irreversible_actions: require }
```
```yaml
# skill.manifest.yaml
apiVersion: amh/v1
kind: Skill
metadata: { name: rotate-log-file, version: 0.3.1 }
spec: { code_ref: skills/rotate_log.py, eval_suite: evals/rotate_log, min_score: 0.9 }
```
```yaml
# connector.manifest.yaml
apiVersion: amh/v1
kind: Connector
metadata: { name: greenhouse-plc, version: 1.0.0 }
spec:
  type: opcua
  endpoint: opc.tcp://10.0.0.5:4840
  auth: mtls
  devices:
    - { id: vent-actuator, actuation_risk: irreversible, bounds: { open_pct: [0, 100] } }
```

### H. End-to-End Worked Scenario

**Goal:** *"Keep the greenhouse healthy overnight; if temperature exceeds 32°C, open the vent, and improve your monitoring over time."*

1. **Decomposition.** An idle-time trigger fires; `pursue_goal` starts (durable). The **lead agent** writes a plan to `write_todos` (persisted): (a) monitor temperature via OPC-UA, (b) act if threshold crossed, (c) log episode, (d) queue a self-improvement reflection.
2. **Sub-agent spawn (context isolation).** Lead spawns a `monitor` subagent (fresh context, objective = "poll vent-actuator temp every 60s, report only threshold crossings") and a `kb` subagent ("retrieve past overnight incidents"). Each returns a **condensed result plus a `trace_ref`**; the lead pulls full traces only during synthesis. Large sensor logs are offloaded to `vfs://runs/2026-08-19/temps.jsonl`.
3. **Context compaction.** After hours of polling, the lead hits 70% budget; the **compactor** summarizes the oldest turns to structured JSON, retains the last 3 turns verbatim, and emits a compaction event — cache-stable prefix preserved.
4. **Device actuation behind an interlock.** Temp hits 33°C. The agent proposes `actuate_device(vent-actuator, {open_pct: 60})`. Because `actuation_risk: irreversible`, an **ApprovalGate** is created. In autonomous overnight mode the gate is pre-authorized within bounds `[0,100]` and rate-limited, so `actuate_device` executes over OPC-UA; a parallel SSH connector confirms actuator status at OS level. Action, bounds, and authorizing policy are written to event history.
5. **Failure injected & self-healed.** The OPC-UA endpoint drops mid-command. The **circuit breaker** for `greenhouse-plc` trips `OPEN`; exponential-backoff retry fires; on the third failure the **supervisor** restarts the connector worker (`one_for_one`) while other agents keep running. Because the step is durable, on reconnection it resumes from the last committed point — no double-actuation (idempotency key on the command). A watchdog confirms recovery; breaker moves `HALF-OPEN` → `CLOSED`.
6. **Self-improvement loop closes.** The sleep-time reflection workflow reviews the episode: the breach was detected 40s late due to a 60s poll interval. **GEPA** proposes a revised monitoring policy (poll 20s when within 3°C of threshold); **ACE** appends the heuristic to the monitoring playbook; a candidate **skill version** is created. It runs the **eval harness** on historical logs, passes a **canary** shadow run, and is **promoted** — versioned, with rollback available. Independent, tamper-proof instrumentation records the improvement; the agent cannot edit its own eval logging.

---

## Recommendations (Staged Roadmap)

**V0 — Walking Skeleton (weeks 1–6).**
- Go daemon as a service on Linux + Windows (supervisor + scheduler + one connector: SSH).
- Python agent layer with the deep-agent harness (planning, VFS, one sub-agent), single model.
- DBOS Transact (SQLite) with `pursue_goal` / `run_subagent` / one durable step.
- SQLite store: ontology tables + episode log + sqlite-vec (extension loaded) + FTS5.
- Context budget manager + compactor + tool-result cap.
- MCP client (stdio) consuming one third-party server; OTel tracing.
- ApprovalGate enforced for one irreversible action.
- **Milestone:** greenhouse scenario steps 1–4 run end-to-end and survive a daemon restart.
- **Defer:** multi-agent beyond 1 sub-agent, self-modification (DGM), Temporal, Qdrant, RDF/OWL, multi-tenant, GUI.

**V1 — Autonomous Habitat (weeks 7–20).**
- Orchestrator-worker (3–5 parallel sub-agents) *only* where sub-problems are independent.
- Full 5-tier memory + bi-temporal KG; sleep-time consolidation.
- Hybrid RAG + GraphRAG + reranking.
- Coder agent (sandboxed CodeAct + TDD).
- Self-improvement: GEPA + ACE + Voyager skills behind eval → canary → rollback.
- Full self-healing (circuit breakers, bulkheads, all LLM failure detectors).
- Connector SDK + REST/GraphQL/webhook + WinRM/MQTT/OPC-UA + Home Assistant.
- MCP server (Streamable HTTP, OAuth 2.1) exposing AMH capabilities.
- Extension/plugin model + stable public SDK for domain apps.

**Thresholds that change the plan:**
- **Vector store:** move off sqlite-vec's brute-force KNN once corpus size or query latency degrades — measure on your own corpus rather than trusting published cross-engine figures.
- **Durable engine:** move DBOS-SQLite → DBOS-Postgres (or Temporal + Postgres) when going multi-replica/multi-tenant.
- **Multi-agent:** enable parallel sub-agents only for breadth-first independent work — the ~15× token cost must be earned.
- **Self-modification:** enable DGM-style edits only after the eval harness + tamper-proof instrumentation are proven on 100+ historical tasks.

**Expansion to multi-user/multi-tenant:** thread a single `tenant_id` column (default `"local"`) through ontology, memory, and durable-workflow keys from day one — cheap now, expensive to retrofit. Add per-tenant isolation (row-level or per-tenant schema), auth (OAuth for the MCP server, API keys for connectors), and DBOS multi-replica when the second user arrives. Do **not** build tenant management UI, RBAC, or sharding in V1.

---

## Caveats & Honest Uncertainties

- **Two facts are [reviewer-sourced] and unverified here:** A2A **1.0.1 (May 2026)** as the current release, and **LadybugDB** as the canonical maintained Kuzu fork. Confirm both before they become load-bearing.
- **The single-agent vs multi-agent debate is unresolved.** Cognition argues parallel sub-agent writers produce compounding miscommunications; Anthropic shows orchestrator-worker wins on breadth-first research at ~15× token cost. The field agrees only on the narrow winner: single-threaded writes plus isolated one-shot sub-calls. AMH defaults to that.
- **KuzuDB was archived in October 2025** after Apple's acquisition. V0 mitigates by modeling the graph as SQLite tables with recursive CTEs; adopt a fork or Oxigraph only if traversal performance demands it.
- **sqlite-vec is pre-v1 and brute-force by default.** It's a deliberate V1 simplification, not a scaling story.
- **Temporal's SQLite persistence is dev/test-only** (#3366 open, no milestone), and `start-dev` doesn't persist at all without `--db-filename`. DBOS docs are slightly inconsistent across SDKs about whether Postgres is "required" vs "recommended" — verify per-language.
- **Restate has no native Windows server binary** as of 1.7.4 — a blocker for an embedded Windows app. **Inngest does ship official Windows amd64/arm64 builds (v1.41.1)**; it was rejected on the server model, not platform support. **Hatchet requires Postgres.**
- **Self-modification carries demonstrable reward-hacking risk.** Independent, tamper-proof evaluation instrumentation and human approval gates are load-bearing safety controls.
- **Benchmark numbers are harness-dependent.** OpenHands' 77.6% SWE-bench figure is its own harness badge; standardized harnesses score lower. Treat all such figures as directional.
- **The Go+Python split adds an IPC seam** (daemon↔agents). A deliberate trade: operational robustness and clean packaging for a gRPC/protobuf boundary to maintain. Revisit if the seam becomes a maintenance burden.
