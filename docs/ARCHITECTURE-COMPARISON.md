# AMH and DeepSeek Harness/Cordis — Independent Architecture Comparison

**Status:** Decision record for architecture selection

This comparison evaluates the systems against the problem they are intended to solve. AMH is not treated as the authority, and DeepSeek Harness is not treated as the authority. The comparison separates habitat capability from harness composition because a habitat contains a harness; it is not reducible to its workflow engine.

## What qualifies as a habitat

An autonomous agent habitat is a continuously operating runtime that hosts agents and gives them the means to persist, reason, use tools, communicate, spawn subordinate work, retain memory, recover from failure, manage resources, and improve under bounded authority. Autonomy does not require zero human intervention; it requires that routine operation, recovery, and lifecycle management proceed without recurring human control. Human approval is an exceptional authority boundary, not evidence that the runtime is non-autonomous.

AMH qualifies as a habitat because its architecture and current repository provide the habitat functions: durable goal execution, cognition workers, memory projections, extension lifecycle, sandboxed computers, policy and trust boundaries, external-effect reconciliation, observability, backup/restore, and self-improvement workflows. The comparison below is therefore not “AMH versus a harness.” It is “AMH as a full habitat versus DeepSeek Harness as a highly composable agent-harness architecture,” with the question of which Cordis mechanisms AMH should adopt.

## Executive verdict

| Question | Better solution | Reason |
|---|---|---|
| Replaceable agent capabilities, runtime composition, live unload, dependency reactivity | **DeepSeek Harness/Cordis** | Cordis makes services, events, and reversible effects first-class composition semantics. The agent loop, model adapter, tools, session log, and UI are all replaceable plugins. |
| Durable workflows, crash recovery, retries, timers, durable signals, and operational replay | **DBOS-backed habitat** | A plugin harness alone does not establish durable workflow ownership or recovery of long-running work. |
| Agent session fidelity, model-visible history, turn replay, and prompt assembly | **DeepSeek Harness/Cordis** | The session event log is the source of model context; messages are derived from events and model-visible input is required to be logged. |
| Domain-specific physical autonomy | **Neither core** | Physical meaning, inverse verification, actuation, reconciliation, and spatial state belong to a domain extension. |
| Minimality and conceptual coherence for a local agent runtime | **DeepSeek Harness/Cordis** | One plugin context and one composition model. |
| Production habitat for autonomous applications | **AMH, strengthened by Cordis semantics** | AMH already supplies the habitat responsibilities. Cordis supplies a stronger composition model for the harness and extension runtime. DeepSeek Harness alone is not a complete habitat. |

The principal architectural error would be to reduce AMH to DBOS plus a thin worker, or to embed DeepSeek Harness as an unexamined second durability authority. AMH remains the habitat: it owns the autonomous operating loop and its lifecycle. The selected design should adopt Cordis's formal composition semantics inside that habitat, while retaining DBOS for durable workflow ownership.

## What is actually being compared

DeepSeek Harness is an agent harness. Its documented architecture is a Cordis plugin tree assembled from profiles, bundles, and patches. The model adapter, tools, session log, agent loop, sandbox, storage, scheduling, and UI are plugins. Cordis supplies dependency injection, typed events, reversible effects, and dependency-reactive activation.

AMH is a continuously operating autonomous habitat. Its problem is not merely composing an agent loop. It hosts the loop and must recover work after process and host failure, retain authoritative durable state, coordinate workers, manage external-effect uncertainty, provide memory and knowledge, supervise extensions, and support self-improvement and self-healing across domains.

Therefore “which is better?” has no single answer until the target is stated. DeepSeek Harness is the stronger harness composition reference. AMH is the stronger full habitat architecture because it includes the autonomous operating environment around the harness. DBOS is one durable execution component inside that habitat, not the definition of AMH.

## First-principles comparison

### 1. Composition model

DeepSeek Harness has the stronger model. “Everything is a plugin” is backed by a shared Cordis context, service definitions, consumers, typed events, and tracked reversible effects. A plugin's registrations unwind when it unloads. Profiles and layered patches make the actual runtime composition inspectable and replaceable.

AMH's previous design described reversible extensions and dependency ordering, but it did not specify the same complete composition calculus. A generic `Effect` record and an extension manifest are not equivalent to Cordis's runtime semantics. AMH should not claim parity by naming the same concepts.

**Decision:** adopt Cordis-compatible temporal and spatial composition semantics as a required AMH extension-runtime contract. Reimplementing only a simplified effect journal is insufficient.

### 2. Durability and recovery

AMH's DBOS choice addresses a requirement that DeepSeek Harness does not claim to solve as its primary architecture: durable workflow execution across worker, daemon, and host failure. A session event log is not by itself a workflow engine. It can reconstruct a conversation, but it does not automatically provide durable timers, child-workflow lifecycle, retry policy, transactional scheduling, or recovery of external-effect state.

DeepSeek Harness's persistence seam and SQLite backends are valuable for session truth, but they should not be promoted into a claim of durable workflow orchestration without acceptance evidence for crash recovery, duplicate suppression, timers, signals, and uncertain external outcomes.

**Decision:** DBOS owns durable goal/turn/workflow orchestration. Cordis owns in-process capability composition. The two are complementary only if neither is allowed to replay or complete the other's state machine.

### 3. Agent-loop ownership

DeepSeek Harness has a cleaner seam: the agent loop is a plugin behind an agent interface, and model-visible inputs are derived from the session event stream. AMH's native harness proposal made the loop a Python service with context middleware, subordinate workflows, and provider logic. That is implementable, but less replaceable and risks turning the habitat into a privileged agent framework.

**Decision:** the agent loop, model adapter, tool registry, prompt assembly, session projection, and subordinate-agent provider are Cordis-style capability plugins. DBOS invokes them through bounded durable turn operations; it does not become the agent loop.

### 4. State and source of truth

DeepSeek Harness has a strong local invariant: model-visible state is logged, and model history is derived from the append-only session log. AMH has a broader state model: workflow state, claims, artifacts, effects, policy decisions, and evaluations must survive beyond a session.

These are different scopes, not competing databases:

- the session event stream is the source for reconstructing one agent's model-visible history;
- DBOS workflow records are the source for durable execution state;
- the habitat store is the source for cross-run claims, artifacts, effects, policies, and evidence;
- extensions own their domain projections and reconciliation state.

No component may silently duplicate another component's authoritative state and then reconcile by convention.

### 5. Reversibility and policy

Cordis provides a concrete mechanism for reversible software composition: effects carry inverses and unload executes them in reverse registration order. This is stronger than a boolean “reversible” field.

Reversibility remains a property that policy may use as a gate. The owning extension defines whether an inverse is meaningful, how it is verified, when it expires, and how recovery is selected. For physical actions, that entire meaning belongs to the Physical AI extension. The AMH core must not derive or invoke a physical inverse.

**Decision:** distinguish three layers:

1. Cordis effect reversibility for runtime composition;
2. extension action reversibility as an attested property;
3. policy decisions that may admit, constrain, defer, approve, or deny based on that property.

Collapsing these layers into one core `Effect` abstraction loses the semantics that make Cordis useful.

### 6. Dynamic composition and self-improvement

DeepSeek Harness is better at live composition. Profiles, bundles, and patches allow a running composition to be inspected and changed without editing the product core. This is exactly the mechanism needed for replaceable models, tools, skills, sandboxes, and loops.

AMH is better at defining a promotion evidence process: evaluate, canary, promote, demote, and roll back with independent evidence. But the promotion mechanism must operate on the actual plugin composition, not only on a version string in a registry.

**Decision:** candidate capabilities are mounted through the Cordis composition layer; DBOS records the promotion workflow and evidence; the evaluator and policy remain outside the candidate's mutation authority.

### 7. Physical and domain extensions

Neither architecture should place physical devices in the platform core. DeepSeek Harness's plugin model is naturally suited to domain ownership. AMH's extension boundary is also correct, but only if it remains a boundary rather than a disguised core schema.

A Physical AI extension may provide `Device`, `DeviceAction`, `Location`, `Pose`, `Mission`, safe states, inverse verification, actuation, reconciliation, and spatial indexes. AMH supplies the generic lifecycle, policy hook, durable workflow, artifact/evidence storage, and plugin composition. The core treats the extension's action as an opaque contract.

### 8. Operational deployment

DeepSeek Harness is more coherent for a local Node/TypeScript harness with web and headless profiles. It has a smaller deployment surface and a direct plugin model.

AMH's Go daemon plus Python workers plus DBOS adds deployment and IPC cost. That cost is justified only by habitat requirements: independent process supervision, cross-platform service operation, durable workflows, connector isolation, and domain extensions that run continuously. If those requirements are removed, DeepSeek Harness is the better and simpler system.

The split must therefore be treated as a requirement-driven choice, not an assumed improvement.

## Recommended target architecture

```text
                    domain extension
              (Physical AI, CRM, finance, ...)
                              |
                       Cordis plugin tree
      (model, tools, skills, loop, session, sandbox, policy adapters)
                              |
                    bounded durable turn
                              |
                   DBOS habitat workflows
       (goals, timers, signals, child work, retry, recovery, evidence)
                              |
                PostgreSQL authoritative store
```

The important boundary is temporal:

- Cordis may mount, activate, quiesce, and dispose capabilities in a worker process.
- Each model turn and external operation is represented by a bounded DBOS operation with durable inputs, outputs, and evidence.
- A crash resumes the DBOS operation from its durable boundary; it does not replay an unbounded live plugin process as if that were workflow recovery.
- External effects use extension-owned idempotency and reconciliation; neither Cordis nor DBOS can infer their outcome.

## Architecture decisions changed by this comparison

1. AMH SHALL adopt Cordis-equivalent effect and dependency semantics, including LIFO disposal, reactive dependency loss, quiescence, and provider replacement.
2. AMH SHALL treat the agent loop as a replaceable capability plugin, not as a privileged habitat subsystem.
3. AMH SHALL retain DBOS for durable workflow orchestration and durable evidence; it SHALL NOT duplicate the session-log or plugin-runtime state machines.
4. DeepSeek Harness may be integrated as a harness profile/adapter if its persistence and process boundaries satisfy the DBOS turn contract. It must not be embedded wholesale until replay, ownership, and failure semantics are proven.
5. The Physical AI system remains an extension. Its entities, spatial data, action inverses, actuation, and recovery are not AMH core semantics.
6. Any comparison claiming AMH is “better” must state the target requirement and evidence. AMH is better for the full habitat objective; DeepSeek Harness is better for the harness-composition objective. Neither claim should be generalized beyond its target.

## Evidence and limitations

The DeepSeek Harness repository is explicitly a developer preview with compatibility-breaking changes expected. Its architecture documentation establishes plugin composition, session events, profiles, bundles, and capability seams; it does not by itself prove production-grade 24/7 durable workflow recovery. AMH's DBOS architecture similarly requires operational qualification rather than assuming that a durable engine makes external effects exactly once.

Primary references:

- [DeepSeek Harness repository](https://github.com/deepseek-ai/deepseek-harness)
- [DeepSeek Harness architecture](https://github.com/deepseek-ai/deepseek-harness/blob/master/docs/architecture.md)
- [DeepSeek Harness core/session documentation](https://github.com/deepseek-ai/deepseek-harness/blob/master/docs/subsystems/core.md)
- [DeepSeek Harness session model](https://github.com/deepseek-ai/deepseek-harness/blob/master/docs/subsystems/session.md)
