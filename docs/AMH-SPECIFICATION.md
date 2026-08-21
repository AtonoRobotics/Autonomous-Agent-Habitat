# Autonomous Multi-Agent Habitat (AMH): Architecture, Written Specification & Concrete Artifacts

*Greenfield platform specification. Spirit-kin to Grok Bot / Hermes / OpenClaw, but fully autonomous and running 24/7. Not a governance app; not a security app; not a physical-robot stack. This is a **platform core** on which domain-specific apps and extensions are built.*

> **Revision note (v2).** Citation and version audit applied. Several claims in v1 were stale, misattributed, or fabricated; they are corrected or removed below.

> **Revision note (v3).** Added §14 (Harness Layer). DeepAgents’ durability model structurally collides with DBOS Transact; AMH reimplements strongest DeepAgents patterns natively on DBOS and does not depend on the `deepagents` package. DeepSeek is a pluggable model tier, not a harness dependency.

> **Revision note (v4).** Added §7a (Spatial Memory) as core schema primitives for stable interfaces; path-planning *algorithms* remain extension responsibility.

> **Revision note (v5).** Added §14.6 (Reversible Capability Composition). Lifted Cordis formal reversibility for software capabilities; rejected ungated live self-modification of the core.

> **Revision note (v6–v8).** Refined reversibility vs physicality gating, earned autonomy, and SafetyCase — **superseded for physical concerns by v9**.

> **Revision note (v9) — 2026-08-21.** Architectural pivot.
> 1. **Physical safety is out of core.** Device interlocks, physical actuation policy, DeviceAction earned autonomy, and physical SafetyCase belong in a **Physical AI extension** that builds on AMH. Treating them as kernel concerns was scope drift.
> 2. **Small hard kernel + swappable modules** is the explicit architecture shape. Features must not be baked into the core by default; they attach via seams and reversible module lifecycle.
> 3. **DeepSeek Harness recommendations absorbed as composition patterns**, not as a runtime dependency: reversible effects, capability seams (definition/provider/consumer), profiles/bundles, reconstructible session/trajectory invariant, optional PTC/Code mode, Creator-style introspection limited to offline/eval-gated jobs.
> 4. **DeepAgents patterns remain the agent-harness base** (native reimplementation on DBOS). DeepSeek Harness is the better teacher for modularity and safe evolution; DeepAgents patterns remain the better agent-loop shape for AMH’s Python layer.
> 5. Core retains **generic** policy/ApprovalGate hooks and software-capability reversibility. Domain policy (physical or otherwise) is supplied by extensions.

---

## TL;DR

- **Python-first agent/LLM layer + Go control-plane daemon**, single-node/single-user for V1, **DBOS Transact (SQLite)** primary durable engine (Temporal dev-server fallback), embedded hybrid store, **MCP + A2A-derived envelope**, context engineering as a first-class subsystem.
- **Context is the scarcest resource:** KV-cache-stable prompts, filesystem offload, sub-agent isolation with result-only return, automatic compaction. Multi-agent used surgically for independent work only.
- **Architecture shape: small hard kernel + swappable modules.** Kernel owns durability, supervision, trajectory reconstructibility, core ontology/store, module lifecycle, and generic policy hooks. Almost everything else is a module or domain extension.
- **Self-improvement and self-healing are eval-gated.** GEPA/ACE/Voyager offline behind canary + rollback; OTP-style supervision, circuit breakers, durable replay. Software capability changes use **effect-tracked reversible composition**. Physical device safety is **not** an AMH core concern.
- **Physical AI is an extension.** Connectors, physical action models, interlocks, and earned autonomy for devices register through AMH seams; they do not expand the kernel.

---

## Key Findings (Decision Summary) — v9

| # | Subsystem | Decision | Primary rationale | Runner-up / fallback |
|---|-----------|----------|-------------------|----------------------|
| 1 | Runtime | **Python (agents) + Go (control plane)** | Deepest agent ecosystem + single-binary daemon, native Windows | Unified TypeScript/Node |
| 2 | Durable execution | **DBOS Transact (SQLite)** | In-process library, no separate server, official Windows support | Temporal dev-server |
| 3 | Architecture shape | **Small hard kernel + swappable modules** | Rapid evolution; avoid baking features into core | Monolithic core |
| 4 | Deep-agent harness | **Native DeepAgents patterns on DBOS** (VFS, subagent isolation, compaction, SKILL.md) — NOT the package | Package durability collides with DBOS; patterns are MIT and right-shaped | Depend on package only if DBOS dropped |
| 5 | Composition / evolution | **Cordis-inspired reversible effects + capability seams + profiles** (from DeepSeek Harness research) | Safe unload/rollback of software modules; config-time composition | Ad-hoc plugin hooks only |
| 6 | Context engineering | **Cache-stable + FS offload + sub-agent isolation + auto-compact** | Anthropic / Manus / Cognition convergence | — |
| 7 | Coder agent | **CodeAct + sandbox + optional PTC/Code mode** | OpenHands/SWE-agent pattern; PTC reduces tool round-trips | Diff-only |
| 8 | Ontology | **Typed property-graph over embedded store** | Pragmatic single-node | Oxigraph if SPARQL needed |
| 9 | Memory | **5-tier + bi-temporal KG** | CoALA taxonomy; Graphiti-style invalidation | Mem0 |
| 10 | Knowledgebase | **Hybrid (vector + BM25 + graph) + rerank** | Embedded, no server | Qdrant/LanceDB at scale |
| 11 | Inter-agent comms | **MCP + A2A-derived envelope over embedded NATS** | Vertical tools + horizontal agents | In-proc bus |
| 12 | Self-improving | **GEPA/ACE offline + Voyager skills, eval-gated** | Safer than unconstrained online RL; fewer rollouts | DSPy MIPROv2 |
| 13 | Self-healing | **OTP supervision + circuit breakers + durable replay** | Battle-tested isolation | Naive retry |
| 14 | Integrations | **MCP client+server, connector SDK; protocol adapters as modules** | Cross-platform reach without hard-coding device policy | — |
| 15 | Observability | **OTel GenAI + durable event replay + token accounting** | Standardized tracing | Custom only |
| 16 | Model routing | **Model-agnostic; DeepSeek as cost tier; frontier for hardest tasks** | Cost/performance flexibility | Single-provider |
| 17 | Spatial | **Optional core Location primitives; algorithms & safety in extensions** | Stable interface without domain policy in kernel | Defer all spatial |
| 18 | Software reversibility | **Effect-tracked, verified-inverse reversibility for capabilities/modules** | Mechanical substrate for eval→canary→rollback | Restart-only |
| 19 | Policy | **Generic ApprovalGate / policy hooks in core** | Extensions supply domain rules (including physical) | No policy surface |
| 20 | Physical safety | **Out of core — Physical AI extension** | Scope correction; keeps core domain-agnostic | (v6–v8 physical gating — revoked for core) |

---

## 0. Kernel vs Modules (normative)

### 0.1 Kernel (hard, stable, rarely changed)

These invariants are difficult to bypass and versioned carefully:

- Durable execution substrate (`DurableEngine` / DBOS) and step-level exactly-once semantics
- Go control-plane supervision tree, health, circuit breakers, bulkheads
- Reconstructible trajectory / session event log (anything model-visible must be reconstructible from the durable log)
- Core ontology primitives and embedded store schema stability contracts
- Module lifecycle (register, start, health, stop, unload) with effect tracking
- Generic policy / ApprovalGate **enforcement surface** (not domain policy content)
- Public SDK / Protocol / seam contracts that modules and extensions depend on
- Isolation boundaries for sandboxes and signed domain packs

### 0.2 Modules and extensions (swappable)

Easy to add, replace, disable, and roll back:

- Agent harness variants (planning style, compaction policy, PTC/Code mode, sub-agent orchestration)
- Memory/RAG backends and tier policies beyond core schema
- Skills, prompts, tool wrappers, Voyager skill libraries
- Self-improvement optimizers and eval suites
- Model providers and routers
- Protocol connectors (SSH, MQTT, OPC-UA, etc. as **modules**, not kernel)
- Observability exporters, UI surfaces
- **Domain packs**, including **Physical AI** (device action models, physical interlocks, SafetyCase for physical effects, spatial planners)

### 0.3 Design rules

1. **Reversibility default for software modules** — mount registers unloadable effects; residual state after unload is a defect.
2. **Seams, not deep inheritance** — Service Definition + Provider + Consumer; swap providers without forking consumers.
3. **Configuration and manifests over code forks** — profiles/bundles/patches compose modules.
4. **Durable state commits through kernel-mediated paths** — modules propose; kernel/store owns commits that affect long-lived habitat state.
5. **Self-modification offline-first and eval-gated** — proposals go through eval → canary → reversible install; agent cannot edit eval instrumentation or kernel interlocks.
6. **Domain safety is extension-owned** — physical, financial, medical, or other vertical policy lives outside the kernel.
7. **Versioned contracts and explicit deprecation** — public seams change slowly; implementations evolve quickly behind them.

---

## 1. Runtime & Language Recommendation

**Primary: Python (agents) + Go (control plane). Runner-up: unified TypeScript/Node.**

| Criterion | Python | Go | TypeScript/Node |
|-----------|--------|-----|-----------------|
| LLM/agent ecosystem | ★★★★★ | ★★ | ★★★★ |
| Durable-execution SDK | ★★★★ (DBOS) | ★★★★★ | ★★★★ |
| Single-binary / Windows daemon | ★★ | ★★★★★ | ★★★★ |
| Concurrency | ★★ (asyncio) | ★★★★★ | ★★★ |

**Process model (single node):**

- `amh-daemon` (Go): supervisor, scheduler, MCP gateway, connector **runtime**, generic policy enforcement, bus, health. systemd / Windows Service.
- `amh-agents` (Python): harness, coder, memory/RAG, self-improvement, module workers. DBOS workers supervised by the daemon.
- `amh-store`: embedded SQLite + sqlite-vec + typed property-graph + durable-execution tables.

Device I/O protocol adapters are **modules** loaded by the connector runtime, not fixed kernel features. Physical safety policy is not in the daemon core.

---

## 2. Durable Execution

**Primary: DBOS Transact (SQLite). Fallback: Temporal dev-server.**

DBOS is an in-process library (`@DBOS.workflow()`, `@DBOS.step()`). SQLite default; Postgres path for multi-replica later. Temporal remains the documented fallback behind a `DurableEngine` port.

**Note:** Temporal SQLite persistence is officially dev/test-only. Restate native Windows server binary was previously unconfirmed/rejected for V1 Windows parity.

---

## 3. Context Engineering

First-class subsystem. Convergent guidance (Anthropic, Manus, Cognition): one coherent agent + isolated one-shot sub-agents over chatty swarms. Multi-agent is surgical because of ~15× token cost on research-style multi-agent evals.

Mechanisms: KV-cache-stable system prompts, hierarchical compaction, filesystem offload as unlimited external memory, sub-agent context isolation with result-only return.

---

## 4. Deep-Agent Harness

**Native reimplementation of DeepAgents patterns on DBOS** — planning tool, VFS, sub-agent isolation, middleware/compaction triggers, SKILL.md-style skills. **Do not depend on the `deepagents` package** (durability collision with DBOS).

**From DeepSeek Harness (composition only, not a dependency):**

- Reversible plugin/effect lifecycle for harness subcomponents where practical
- Reconstructible trajectory invariant (model-visible content reconstructible from durable log)
- Optional **PTC / Code mode** for the coder agent (model emits a program that orchestrates multi-step tool use)
- **Profiles** for composing model + tools + context policy + sub-agents
- Creator-style introspection **only** in offline / sleep-time / eval-gated jobs — not live hot-swap of the outer kernel

Harness implementations remain **modules** behind a stable Agent/Harness seam so alternate harnesses can be swapped without rewriting the habitat.

---

## 5. Coder Agent

CodeAct-style loop + sandbox + diff apply + TDD verify. Optional PTC/Code mode as a module feature. Event-sourced replay via durable engine.

---

## 6. Ontology, Memory, Knowledgebase

- Ontology: typed property-graph over embedded store (not RDF/OWL in V1).
- Memory: 5-tier (working / episodic / semantic / procedural / entity) + bi-temporal KG.
- KB: hybrid vector + BM25 + graph + rerank (sqlite-vec + FTS5).

Spatial: core may expose Location + bi-temporal `located_at` primitives for stable interfaces; **path-planning algorithms and physical safety policy are extension-owned**.

---

## 7. Inter-Agent Communication

MCP (agent↔tool) + A2A-derived internal envelope (agent↔agent) over embedded NATS/blackboard. Envelope carries `context_ref` for isolation, not full parent context dumps.

---

## 8. Self-Improvement & Self-Healing

**Self-improvement:** GEPA/ACE offline reflection + Voyager-style skills, behind independent eval instrumentation → canary → promote/rollback. Prefer verifiable trajectory rewards and group-relative candidate comparison inside the eval harness. **Avoid unconstrained online RL on the live habitat.**

**Self-healing:** OTP-style supervision in Go, circuit breakers, bulkheads, durable workflow replay. Capability-level granularity for software modules via reversible unload.

**Reward-hacking control:** eval tooling is independent and not editable by the agent under test.

---

## 9. Policy Surface (core)

Core provides:

- `ApprovalGate` / policy **hook** API
- Audit logging and rate-limit primitives
- Effect registry for software capability mount/unload

Core does **not** define physical risk classes, device interlocks, or SafetyCase evaluation for physical effects. Extensions bind domain policy to the hook.

---

## 10. Physical AI Extension (non-normative for core; normative for the extension pack)

Ships as a domain pack / extension, not as AMH kernel:

| Concern | Owner |
|---------|--------|
| Device connectors (SSH, WinRM, MQTT, OPC-UA, …) | Extension modules on connector runtime |
| Physical action model & verified inverse for actuation | Extension |
| Interlocks / when to invoke ApprovalGate for physical effects | Extension policy |
| Earned autonomy / SafetyCase for devices | Extension |
| Spatial planners, navigation, manipulation safety | Extension |
| Greenhouse / cinema robotics / industrial packs | Extension domain packs |

AMH core guarantees: durable steps, module lifecycle, generic policy enforcement, trajectory log, isolation. The extension supplies *what* is safe for physical work.

---

## 11. Observability

OpenTelemetry GenAI conventions, durable event replay, token accounting. Trajectory log is the source of truth for model-visible history (DeepSeek-style invariant).

---

## 12. Model Routing

Model-agnostic provider interface. DeepSeek (and similar) as cost-efficient tier via adapter (`reasoning_content` round-trip handling as needed). Frontier models for hardest long-horizon tasks.

---

## 13. DeepSeek Harness vs DeepAgents (decision record)

| Role | Choice |
|------|--------|
| Agent harness **patterns** | Native DeepAgents-style (planning, VFS, sub-agents, middleware) on DBOS |
| Composition / evolution **patterns** | Cordis / DeepSeek Harness (reversible effects, seams, profiles, trajectory invariant) |
| Runtime dependency | **Neither** dsh nor `deepagents` package as hard dependency |
| Physical / habitat kernel | AMH-owned; neither project replaces it |

DeepSeek Harness does not ship an RL training feature. Related research (harness evolution via outcome rewards, GRPO for models) informs **eval design**, not a live online RL loop in the habitat.

---

## 14. V0 / V1 Roadmap (adjusted)

**V0 — walking skeleton**

- Go daemon supervisor + health
- Python agents on DBOS/SQLite
- Context budget manager
- MCP client
- Module lifecycle stub + one example software module
- Generic policy hook (no physical interlock required)
- OTel tracing
- Non-physical demo workflow survives restart

**V1 — autonomous habitat**

- Orchestrator/worker multi-agent (3–5 sub-agents for independent work)
- Full 5-tier memory + bi-temporal KG
- Coder agent + sandbox (+ optional PTC)
- Self-improvement (GEPA/ACE/Voyager) behind eval/canary/promote
- Reversible software capability composition
- Connector SDK + MCP server
- Extension/plugin model with signed packs
- **Physical AI extension** as separate pack (connectors + physical policy), not blocking core V1

---

## 15. Artifacts (stable public surface)

Retain and evolve:

- **A** Monorepo layout (see README; `extensions/physical-ai/` is extension, not kernel)
- **B** Protocol ports (`Agent`, `Tool`, `VectorStore`, `MemoryStore`, `DurableEngine`, `ApprovalGate` as generic hook, `ModuleLifecycle`)
- **C** Ontology schema (core entities; device/physical entities live in extension schemas)
- **D** Inter-agent envelope JSON schema
- **E** Memory & KB DDL
- **F** Durable workflow examples (goal pursuit, sub-agent, generic actuate-via-policy — not hard-coded device safety)
- **G** Manifests (agent, skill, connector, **extension pack**)
- **H** Worked scenario: non-physical autonomous loop in core docs; physical greenhouse scenario lives in Physical AI extension docs

---

## 16. Caveats (updated)

- Do not reintroduce physical safety into the kernel under another name.
- Do not depend on `deepagents` or DeepSeek Harness as the durable outer runtime.
- Benchmark numbers are harness-dependent; treat as directional.
- Self-modification without independent eval instrumentation is a reward-hacking risk.
- Module seams add an IPC/API boundary to maintain; that is the deliberate cost of evolvability.
- Temporal SQLite is not production persistence; DBOS remains primary.

---

**Alpha Vector LLC.** Specification **v9 (2026-08-21)**.
