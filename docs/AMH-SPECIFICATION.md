# Autonomous Multi-Agent Habitat (AMH) — Governing Architecture

**Status:** Authoritative production baseline

**Revision:** 10

**Date:** 2026-08-21

AMH is a continuously operating habitat for autonomous agents. It wakes from goals, schedules, events, and messages; supplies durable work, context, memory, tools, workers, and recovery; and exposes stable seams on which domain applications are built.

AMH is not a governance product, a security product, a physical-device stack, or a domain application. Operational controls exist because a 24/7 runtime needs them, but domain meaning and domain safety remain outside the core.

## 1. Governing decisions

1. **Small hard core, reversible extensions.** The core contains only invariants that every domain requires. Models, harness policies, tools, connectors, memory implementations, user surfaces, and domain behavior attach as replaceable extensions.
2. **Python cognition, Go habitat daemon.** Python owns model-facing cognition. Go owns service supervision, extension hosting, local transport, resource isolation, and connector process I/O.
3. **DBOS is the sole durable workflow engine.** DBOS Transact uses PostgreSQL as the sole, day-one authoritative store — not a smaller database staged for later replacement. Temporal is a documented migration alternative, not a configuration-equivalent fallback.
4. **PostgreSQL is authoritative persistent state, from day one.** DBOS workflow history, AMH records, extension registration, memory, knowledge, and evidence are stored or projected from PostgreSQL. Neither an agent, NATS, nor an in-memory supervisor owns truth.
5. **Context is a managed runtime resource.** Budgeting, offload, retrieval, compaction, cache discipline, and isolated subordinate contexts are first-class services.
6. **Cordis spatiotemporal composition governs extension lifecycle.** Temporal composability tracks reversible software effects. Spatial composability declares provider/consumer dependencies and determines activation and teardown order. Cordis “spatial” is dependency composition, not physical geometry.
7. **Reversibility is a policy property.** The core can evaluate declared, attested action properties through generic policy hooks. The extension that defines an action owns the meaning, verification, inverse, recovery, and domain policy for that action.
8. **Physical devices belong to a Physical AI extension.** The AMH core contains no `Device`, `DeviceAction`, physical `Location`, physical `SafetyCase`, actuation workflow, or device recovery policy.
9. **Agents propose; deterministic services commit.** Models may reason, plan, retrieve, synthesize, and propose actions. They do not directly mutate durable workflow state, extension registration, policy decisions, or evidence.
10. **Self-change is offline, evaluated, canaried, and reversible.** The live system cannot promote its own candidate or alter its evaluator, evidence, policy hook, or promotion threshold.

## 2. System boundary

### 2.1 Core responsibilities

The AMH core SHALL provide:

- durable workflow start, wait, signal, cancellation, child execution, retry, and recovery;
- process and worker supervision;
- extension discovery, dependency resolution, activation, quiescence, disposal, and rollback;
- effect journaling for core-mediated software composition effects;
- generic action admission and approval-hook execution;
- scoped artifact and context storage;
- core identity, goal, task, run, event, message, artifact, memory, claim, extension, capability, effect, evaluation, and version records;
- model-provider and tool-provider seams;
- MCP client/server interoperability;
- A2A interoperability at the external agent boundary;
- OpenTelemetry tracing and per-goal/run/model cost accounting;
- independent evaluation, canary, promotion, demotion, and rollback mechanics.

### 2.2 Extension responsibilities

An extension SHALL own all semantics it introduces, including:

- action schemas and effects;
- domain entities and relations;
- domain policy and approval requirements;
- inverse construction and verification;
- reconciliation and recovery selection;
- connector-specific idempotency and uncertain-outcome handling;
- domain telemetry, health interpretation, and acceptance evidence;
- domain storage projections and indexes;
- domain-specific safety cases or assurance arguments.

### 2.3 Explicitly outside the core

- robot control, navigation, manipulation, PLC semantics, or device safe states;
- financial, medical, legal, or real-estate policy;
- physical coordinate frames, maps, poses, paths, or R-tree schemas;
- domain risk taxonomies;
- domain-specific approval rules;
- derivation of an inverse for an arbitrary physical or external action.

The core MAY host generic extension-provided schemas and indexes. Hosting does not transfer semantic ownership to the core.

## 3. Runtime ownership

### 3.1 Go daemon

`amh-daemon` owns:

- operating-system service lifecycle;
- worker process start, health, restart, and termination;
- extension host isolation;
- local gRPC transport;
- resource limits and bulkheads;
- MCP transport gateway;
- connector subprocess and socket I/O;
- model-provider and tool-provider credential custody and inference request routing;
- deterministic delivery of external triggers into DBOS using stable idempotency keys.

The daemon SHALL NOT independently advance, retry, or complete a DBOS workflow. Restarting a process is not permission to repeat its durable operation.

### 3.2 Python cognition workers

`amh-agents` owns:

- provider-neutral model calls, issued through the daemon's inference seam using only its own agent identity — a cognition worker holds no model-provider API key, OAuth token, or base URL of its own;
- context assembly and compaction;
- planning and subordinate-agent cognition;
- retrieval and memory consolidation;
- coder-agent reasoning inside a sandbox;
- candidate prompt, skill, routing, and capability generation;
- offline evaluations.

Cognition workers SHALL be replaceable. All state required to resume a durable run SHALL be reconstructible from durable records and scoped artifacts.

### 3.3 DBOS and PostgreSQL

DBOS owns the durable workflow lifecycle. PostgreSQL owns persisted truth.

The system SHALL use:

- DBOS queues/signals for durable workflow communication;
- transactional inbox/outbox records for durable external delivery;
- local gRPC for synchronous daemon/worker calls;
- in-process channels only for disposable notification.

NATS is not a core dependency. A NATS/JetStream transport extension MAY be added when multi-process fan-out or multi-node topology justifies it.

PostgreSQL admission requires crash, power-loss, WAL recovery, disk-full, backup/restore, migration-with-pending-work, lock-contention, and Windows-service qualification. Failure of that qualification is a durability defect to fix, not license to weaken durability or fall back to a lesser store.

Temporal is a migration target behind the AMH operation contract. Migrating requires workflow semantic compatibility tests and state migration; it is not a configuration swap.

## 4. Durable operation and external effects

AMH guarantees durable orchestration, not universal exactly-once external effects.

Core-local transactional steps MAY be exactly-once when the application write and durability record commit atomically in the same database transaction. Calls to MCP servers, APIs, connectors, devices, filesystems, or other processes are external effects and SHALL use the following generic lifecycle:

```text
PROPOSED
  -> ADMITTED | REJECTED | NEEDS_APPROVAL
  -> DISPATCH_PENDING
  -> DISPATCHED
  -> OBSERVED | OUTCOME_UNKNOWN
  -> CONFIRMED | RECONCILED | COMPENSATED | FAILED
```

Before dispatch, the owning extension SHALL supply:

- stable operation and command identities;
- canonical payload digest;
- retry classification;
- idempotency mechanism, if available;
- observation/reconciliation method;
- timeout and uncertainty behavior;
- declared properties used by policy, including reversibility where applicable.

After an interrupted dispatch, AMH SHALL ask the extension to reconcile. AMH SHALL NOT infer that the effect failed, retry blindly, construct an inverse, or select a domain recovery action.

## 5. Extension composition

### 5.1 Cordis temporal composability

Every core-mediated software effect installed by an extension SHALL register its disposer at the time the effect is created. Effects SHALL be disposed in reverse registration order.

Examples include:

- tool registration / tool removal;
- event subscription / unsubscribe;
- timer start / timer stop;
- route registration / route removal;
- service publication / withdrawal;
- child extension mount / child extension disposal.

An effect that bypasses the extension context is outside the reversibility guarantee and SHALL be rejected by conformance tests for an extension claiming reversible unload.

### 5.2 Cordis spatial composability

Extensions SHALL declare capabilities they provide and require. The resolver SHALL:

- activate providers before consumers;
- reject unsatisfied required dependencies;
- detect dependency cycles;
- quiesce consumers before withdrawing their provider;
- dispose consumers before providers;
- reactivate consumers only after replacement providers pass health and compatibility checks.

### 5.3 Extension lifecycle

```text
DISCOVERED -> VALIDATED -> RESOLVED -> STAGED -> ACTIVATING -> ACTIVE
ACTIVE -> QUIESCING -> DISPOSING -> DISPOSED
ACTIVATING | ACTIVE | DISPOSING -> FAILED -> RECOVERY_REQUIRED
```

Activation and disposal progress SHALL be journaled incrementally. An extension SHALL NOT return an unpersisted list of completed effects only after activation finishes.

### 5.4 Reversibility property

`reversibility` is a generic action property with three values:

- `verified`: the owning extension supplies a current verification attestation and inverse contract;
- `claimed`: an inverse is declared but not currently attested;
- `none`: no inverse is available.

The core policy surface MAY use this property as a gating predicate. The extension owns what the property means for its action and SHALL bind its attestation to the extension version, action schema version, verification evidence, validity conditions, and invalidation rules.

The existence of an inverse does not instruct the core to invoke it. The owning extension selects recovery through its reconciliation contract.

## 6. Generic policy and approval seam

The core SHALL provide a fail-closed policy hook without embedding domain rules.

Input:

- principal and extension identity;
- action type and schema version;
- canonical payload digest;
- declared properties and attestation references;
- requested deadline and resource budget;
- extension-provided domain context reference.

Output:

- `admit`;
- `admit_with_constraints`;
- `needs_approval`;
- `defer`;
- `deny`.

The result SHALL bind the exact action digest, constraints, policy version, decision time, and expiry. Check-then-act approval is forbidden: dispatch consumes the bound decision atomically or fails closed.

Physical AI, finance, medical, communications, deployment, and other extensions define their own policies over this seam.

## 7. Agent harness and context

AMH SHALL implement the useful DeepAgents patterns natively on DBOS and SHALL NOT depend on LangGraph or the `deepagents` package while DBOS remains the durability engine.

Required harness services:

- tool-calling loop;
- scoped VFS/artifact tools;
- isolated subordinate-agent workflow;
- configurable planning/recitation tool;
- context budget manager;
- compaction middleware;
- SKILL.md progressive disclosure;
- provider-neutral prompt-cache policy;
- OpenTelemetry spans.

Context rules:

1. Stable, deterministic prompt prefix where the provider supports prefix caching.
2. Configurable per-provider cache behavior; no assumption that hosted APIs expose token-level logit masking.
3. Opaque, non-enumerable, capability-scoped artifact handles rather than ambient filesystem paths.
4. Large tool output stored externally and retrieved by range/filter.
5. Single-result cap, default 25,000 tokens unless the provider/model limit is lower.
6. Compaction preserves governing constraints, goal, completion predicate, unresolved decisions, failures, uncertainty, active plan, and artifact references.
7. Most recent turns remain raw according to provider policy.
8. Subordinate agents receive explicit objective, boundary, artifact grants, budget, and output schema.
9. Subordinate traces are durable artifacts available to the manager but are not automatically injected into any agent context.

DeepSeek thinking-mode adapters SHALL preserve required `reasoning_content` across the active tool-call chain. Once the chain completes, provider policy determines retention and compaction.

## 8. Memory and knowledge

The core ontology contains only domain-neutral records:

- Principal, Agent, Goal, Task, Run, Event, Message, Artifact;
- Extension, Capability, ActionType, Effect, PolicyDecision, ApprovalRequest;
- Memory, Claim, EntityRef, SkillVersion, PromptVersion, Eval, CandidateVersion.

Memory projections:

- working: active task/context projection, derived at query time from the core ontology (goal/task/run/event) in PostgreSQL — not a separate store;
- episodic: event projection, backed by Hindsight (self-hosted, schema-isolated within the same PostgreSQL cluster as the rest of AMH's authoritative state);
- semantic: bi-temporal claim graph, backed by Neo4j via Graphiti;
- procedural: versioned skills and playbooks, the core ontology's `skill` table in PostgreSQL;
- entity: profiles assembled from claims and evidence, backed by Neo4j via Graphiti (entity nodes and their edges).

Semantic claims SHALL preserve both valid time and transaction time, provenance, confidence meaning, contradiction/supersession links, and extraction version — Graphiti's bi-temporal edges (`created_at`/`expired_at`/`valid_at`/`invalid_at`) and LLM-driven contradiction resolution satisfy this directly.

Knowledge retrieval SHALL combine lexical, vector, and graph retrieval behind ports. PostgreSQL (authoritative state) and Neo4j (semantic/entity graph) are the permanent backends behind those ports, not placeholders staged for later replacement: Hindsight's `recall`/`reflect` operations provide lexical+vector retrieval over episodic memory, and Graphiti's `search` provides graph retrieval over the semantic/entity graph, fused at the retrieval port. Embedding identity, dimension, model version, chunk version, source digest, and ACL/visibility metadata SHALL be stored with every vector.

Domain extensions MAY register namespaced entity schemas, relations, indexes, retrieval enrichers, and consolidation rules. Physical geometry and spatial indexes belong to the Physical AI extension.

## 9. Interoperability

- **MCP 2026-07-28** is the native tool/resource interoperability baseline. Older MCP versions are compatibility adapters.
- **A2A 1.0** is the external agent interoperability baseline.
- AMH internal durable communication uses its own versioned operation/message contracts rather than an “A2A-derived” private envelope.

MCP and A2A adapters SHALL translate external lifecycle and errors into AMH canonical operation states without becoming workflow authorities.

## 10. Self-improvement

Candidate classes are evaluated independently:

| Candidate | Required evidence |
|---|---|
| Prompt | quality, cost, latency, regression suite |
| Retrieval policy | relevance, provenance, poisoning, latency |
| Declarative skill | schema plus scenario execution |
| Executable skill/module | sandbox, static checks, tests, capability and effect conformance |
| Core code | full build, migration, compatibility, durability, recovery, and operational qualification |

Promotion flow:

```text
GENERATED -> EVALUATED -> CANARY -> PROMOTED | REJECTED
PROMOTED -> DEMOTED -> ROLLED_BACK
```

GEPA, ACE, Voyager-style skill extraction, DSPy optimizers, and coder agents are replaceable modules. No optimizer may alter its evaluator, held-out cases, instrumentation, policy decision, approval, or promotion threshold.

Promotion of a runtime capability uses the extension lifecycle: stage candidate, activate canary, observe independent evidence, quiesce previous provider, switch binding, retain rollback target, and dispose only when the rollback window closes.

## 11. Self-healing

Recovery ownership is explicit:

| Failure | Owner |
|---|---|
| Go daemon or worker process | OS/service supervisor |
| Python cognition worker | Go supervisor; DBOS resumes durable workflow |
| Durable workflow interruption | DBOS |
| Provider outage/rate limit | provider router/circuit breaker |
| Extension process failure | extension supervisor |
| Extension dependency loss | composition resolver |
| External effect uncertainty | owning extension reconciler |
| Context overflow | context manager |
| Invalid model/tool output | contract validator |
| Goal cannot continue | durable goal workflow |

Restarting a process restores availability; it does not prove an operation succeeded. Recovery SHALL restore invariants or explicitly retain `OUTCOME_UNKNOWN`.

## 12. Public contracts

The following repository artifacts are normative:

- `contracts/extension-manifest.schema.json`
- `contracts/action-envelope.schema.json`
- `contracts/effect-record.schema.json`
- `contracts/policy-decision.schema.json`

Domain extensions SHALL publish their schemas under their own namespace and SHALL NOT modify AMH core schemas to introduce domain entities.

Public contracts require semantic versioning, compatibility declarations, typed errors, concurrency/version tokens where mutable state is involved, and deprecation periods.

## 13. Physical AI extension boundary

The separate Physical AI extension owns:

- Device, DeviceAction, Location, Pose, Frame, Map, Mission, SafetyCase, and physical Effect specializations;
- SSH, WinRM, MQTT, OPC-UA, Modbus, ROS 2, and device-driver modules used by that extension;
- inverse verification for physical actions;
- actuation bounds, interlocks, safe states, recovery, and earned-autonomy policy;
- geometry, R-tree or other spatial indexes;
- greenhouse, robot, industrial, cinema-motion-control, or building-control scenarios.

The extension uses AMH durable workflows, generic action envelope, policy hook, effect record, artifacts, memory projections, and extension lifecycle. The core never interprets a physical command.

## 14. Deployment

There is no staged rollout: every item below is day-one production scope, not a later phase.

- Go daemon as Linux systemd and Windows Service;
- Python cognition workers managed by `uv` and packaged per target OS;
- DBOS/PostgreSQL durability qualification;
- scoped artifact/VFS service and context budget manager;
- provider routing and failover across model providers, and one MCP 2026-07-28 client;
- extension resolver and reversible software effect journal;
- generic policy hook;
- OpenTelemetry tracing;
- non-domain autonomous workflow surviving process and host restart;
- isolated subordinate-agent workflows with bounded concurrency;
- five memory projections and hybrid retrieval;
- coder agent and production sandbox;
- MCP server and A2A 1.0 adapter;
- independent eval/canary/promotion/rollback;
- connector and domain-extension SDKs;
- signed extension packs and compatibility qualification;
- backup/restore, upgrade/rollback, corruption recovery, resource exhaustion, and soak acceptance.

Physical AI is a separate extension, designed, built, and released independently of the AMH core — not a later phase of it.

## 15. Acceptance invariants

AMH is not complete until automated evidence proves:

1. a durable workflow resumes after worker, daemon, and host restart without duplicate committed steps;
2. interrupted external effects enter reconciliation and can remain `OUTCOME_UNKNOWN`;
3. removing a conformant reversible extension leaves no registered context-mediated effects;
4. dependency removal quiesces and disposes consumers before providers;
5. an extension cannot mutate another extension’s owned effects;
6. policy dispatch is bound to the admitted action digest and fails closed after expiry or mutation;
7. a subordinate agent cannot enumerate artifacts outside its grants;
8. compaction preserves every required checkpoint field;
9. the trajectory presented to a model is reconstructible from durable records and artifacts;
10. a candidate cannot modify its evaluator, evidence, policy, or promotion threshold;
11. rollback restores the prior capability binding and compatible persisted state;
12. no core schema contains domain-owned physical entities or policy.

## 16. Superseded decisions

The following earlier decisions are revoked and SHALL NOT be reintroduced:

- physicality or physical safety as an AMH core concern;
- core `Device`, `DeviceAction`, physical `Location`, physical `SafetyCase`, or greenhouse workflow types;
- generic core derivation or invocation of physical inverses;
- LangGraph/DeepAgents checkpointer nested inside DBOS workflows;
- DBOS-to-Temporal described as a configuration swap;
- NATS as a mandatory core dependency;
- private “A2A-derived” internal envelopes presented as A2A compatibility;
- universal exactly-once claims for external effects.

This document supersedes earlier AMH architecture revisions.
