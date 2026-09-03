# cortexd — Cognition Runtime Specification

Status: draft 1.

> Read `DESIGN.md` first. This spec is a contract: the **Guarantees** are what other services and tests depend on and must hold exactly. Mechanisms described under them are the reference approach; a builder may choose differently if every guarantee and test still holds. Thresholds and defaults are in `habitat.config` and referenced by name here; never hard-code them.


## 1. Purpose

`cortexd` is the nervous system between the agent's body (the OS, exposed through `ontd`) and its mind (a model). It owns the agent loop: waking an agent, assembling its context, running its cognition against a model backend, exposing the actions its charter and policy allow, running lifecycle hooks, compressing long sessions, enforcing cognition budgets, and closing the session into the journal.

It is a system service like any other, with its own uid, and it serves agents as clients. It never decides *what* an agent should do. It does not evaluate policy; `ontd` does. It does not run workflows; `wfd` does. It does not classify or filter model output.

Humans do not use `cortexd`. A human's runtime is their body and the shell.

## 2. Guarantees

Why: cognition is expensive and must not be wasted or hidden. Context is assembled by code before the model is called, all model calls go through the front door, and nothing about pressure is ever visible to the model.

1. An agent sees exactly three things: ontology reads, typed observations and yields, and typed actions. No raw filesystem, process, or network access from cognition. Everything else is behind a module.
2. Working context is assembled to completion before the model is called. No wake proceeds on partial context.
3. Nothing about context pressure, budget pressure, or compression is ever visible to the model.
4. Compression rebuilds context from the ontology and journal. The rolling summary is one input, never the sole carrier of task state.
5. Every hook is a module with a manifest, sandbox, and fixtures. A hook that needs judgment obtains it by spawning a sub-agent with a result schema; it never calls a backend directly. All cognition is a wake or a sub-agent job: attributed, budgeted, journaled, and limited to the agent interface.
6. Every model call is attributed to a uid and charged to a budget. Sub-agent cognition is charged to the root ancestor.
7. The model backend is replaceable with zero change to any agent, charter, hook, or session record.
8. Model output is never executed. It is parsed into typed calls against `ontd` or discarded with a journaled parse failure.

## 3. Agent interface

The entire surface an agent's cognition can reach, exposed as a tool schema to the model and enforced by `cortexd`:

```
read(ObjectId | query)                      -> objects, links     # ontd read API, charter-scoped contexts
context()                                   -> assembled context   # the current wake's working context, re-readable
act(verb, target, args)                     -> ActionInvocationId  # ontd.act; policy evaluated there
resolve(YieldId, answer, rule?)             -> ok                   # ontd.resolve
spawn(result_schema, ttl, context, task)    -> SubAgentJobId        # registryd.SpawnSubAgent
remember(fact: TypedStatement)              -> ok                   # write to private memory
recall(query)                               -> facts                # private memory retrieval step
done(summary: Object)                       -> ends the wake
```

`read` is scoped to the contexts in the agent's charter plus `habitat`. `act` presents only the verbs the charter declares and policy permits for the agent's groups, narrowed further for sub-agents to the verbs in their job. Anything not listed is not a tool and does not exist from the model's point of view.

There is no shell tool, file tool, or HTTP tool. Those are actions in the `os` and `interface` contexts, exposed only to agents whose charter and groups include them.

## 4. Wake lifecycle

A wake is triggered by a routed observation, a routed yield, a sub-agent job assignment, or a human attaching a conversation. Each wake is a `Session` object.

```
1. trigger arrives on ont.habitat.yield.<uid> or ont.<ctx>.observation.<Type> (dispatcher-routed)
2. pre-wake hooks run (working-context assembly and any charter-declared hooks)
3. session created; parent link set if this is a compression child
4. loop:
     model call with system frame + context + tool schema
     parse output into typed calls
     for each act(): pre-act hooks -> ontd.act -> post-result hooks
     append turn to session
     if compression threshold reached: on-compress hook (§7), continue in child session
5. done() or budget exhaustion or idle timeout
6. post-wake hooks run (journal, memory sync, outcome recording)
7. session closed; observation/yield outcome written
```

An agent that is not woken is not running. Idle agents cost nothing. `cortexd` keeps no warm state for them beyond the NSS projection.

## 5. Working context

Assembled by the `habitat/AssembleContext` step, run in the sandbox before the model call. Deterministic; same inputs produce the same context. Contents, in order:

1. **Charter frame.** The agent's contexts, owned types, available verbs. Static per charter version; cached and prompt-cache-stable.
2. **Trigger.** The observation or yield, with its `question_type` and enumerated options if any.
3. **Neighborhood.** `ontd.neighborhood(target, cortexd.neighborhood_depth)`. Fixed depth. Additional context beyond two hops arrives only via explicit steps in the workflow that raised the yield.
4. **Reliability annotations.** For every sensor and module in the neighborhood, its precision or reliability score. The agent is told how much to trust what it's looking at.
5. **Private memory.** Output of the retrieval step (§8) for the trigger, exact matches first, then approximate if the step is configured for it, each marked with trust and match kind.
6. **Rolling summary.** If this is a compression child, the summary object from the parent session.
7. **Open items.** The agent's other unresolved yields and running sub-agent jobs, as references.

The frame is built so that stable content precedes volatile content, for prompt caching. Section 1 changes only with charter version; section 2 onward is per wake.

Context size is bounded by `cortexd.context_max_tokens`. If the assembled context exceeds the bound, the step fails the wake with `ContextOverflow`, emitted as an observation on the workflow that raised the trigger. That is a bug in the workflow's yield design, not something to truncate around.

## 6. Hooks

Triggers start wakes; hooks run inside them at lifecycle points. A hook is deterministic code. Where a hook needs judgment about something *other than the decision the agent just made* (summarizing a session, extracting facts from a long result), it calls `spawn()` with a result schema and continues on the typed result. The model is reached only through that path, so hook cognition is charged, journaled, and tool-limited like any other.

A hook never re-evaluates the agent's own decision. Pre-act hooks are deterministic checks only: contracts on arguments, version match on the target, facts unchanged since context assembly. A failed check returns to the agent as a typed result in the same wake; the agent then decides with new information, which is a different decision, not the same one twice. An agent that thinks twice about one action is a wake whose context was incomplete, and the fix is the context, not a checker.

Hook points, each a list of module ids declared in the charter, run in order, each in its own sandbox with declared inputs only:

| point              | inputs                             | outputs                      | when                                   |
|--------------------|------------------------------------|------------------------------|----------------------------------------|
| pre-wake           | trigger                            | context additions            | before model call, after AssembleContext |
| pre-act            | pending action, context            | allow / reject with reason   | before ontd.act; deterministic only    |
| post-result        | action result                      | context additions, memory writes | after result returns              |
| on-yield-resolved  | yield, answer, rule                | crystallization candidates   | after resolve                          |
| on-compress        | session, summary so far            | new summary object           | at compression threshold               |
| on-budget-threshold| remaining budget                   | scope adjustments            | silent; never surfaces to the model    |
| post-wake          | session                            | journal entries, memory sync | after done()                           |

Seed hooks, always present and not removable by charter:

- `habitat/AssembleContext` (pre-wake)
- `habitat/RecordOutcome` (post-wake): writes observation and yield outcomes, feeding precision scores
- `habitat/CaptureRule` (on-yield-resolved): stores the stated rule against the yield point for `YieldRecurrence`
- `habitat/SyncMemory` (post-wake): writes `remember()` calls to private memory, enforcing the cap (§8)

A hook that fails rejects the wake step it guards and emits `HookFailed` on the hook's module object. Hooks are modules and so have reliability scores; a hook whose failure rate rises wakes its owner.

## 7. Sessions and compression

Sessions are lineage, not a rewritten transcript. When the session's token count reaches the `cortexd.compression_soft` of the backend's window:

1. The `on-compress` hook spawns a sub-agent with result schema `habitat/Summary`, passing the prior summary and the turns since; the child returns an updated summary, never one rebuilt from scratch. The job is charged to the agent and journaled like any other.
2. The current session is closed with `ended` and the summary linked.
3. A child session is created with `parent` set.
4. `AssembleContext` runs again from `ontd` and the journal, with the summary as section 6. The child's context is rebuilt from the world, not inherited from the parent's transcript.
5. The loop continues in the child.

A hard safety net at `cortexd.compression_hard` forces compression regardless of turn boundaries.

The model is never told compression happened, is happening, or is near. There are no pressure warnings.

Raw turns are retained in the journal for every session and are recoverable by session id; nothing is lost, only removed from the active window.

## 8. Private memory

Each agent's private memory is a typed fact store in its home, written only through `remember()` (via `SyncMemory`) and by post-result hooks that extract facts by rule. No model summarizes the journal into memory.

```
Fact {
  id, statement: TypedStatement       # subject: ObjectId, predicate: string, object: value | ObjectId
  trust: float                         # computed: led to acted outcome / times retrieved
  source: SessionId, at
  superseded_by: FactId | null
}
```

**Cap.** Private memory has a hard size limit, `agents.private_memory_cap_facts`. `SyncMemory` refuses writes past the cap and emits `PrivateMemoryFull`, which routes to the agent as a standing work item: crystallize or supersede. Growth toward the cap is a sensor. The cap exists to force knowledge into the ontology where it can be shared and tested.

**Retrieval** is the `habitat/RecallFacts` step: typed lookup by subject and predicate first, full-text second, approximate third if configured. Each returned fact is marked with its match kind and trust. This step is replaceable; an HRR-based store is an acceptable implementation.

**Trust** is computed by `RecordOutcome`: a fact that was in context when an action's outcome was `acted` gains; one present when the outcome was `dismissed` or a contract violation followed loses. Facts below `agents.private_memory_trust_floor` are superseded automatically and surfaced.

Sub-agents have no private memory. They get working context and return a typed result.

## 9. Budgets

`CognitionBudget` from `registryd` is enforced here as a per-uid rate limit on tokens and GPU-seconds per day. Sub-agent calls are charged to the root ancestor's uid.

On crossing `budget.threshold_warn` and `budget.threshold_scope`, the `on-budget-threshold` hook runs and may narrow scope: reduce neighborhood annotations, skip approximate recall, decline new sub-agent spawns. The model is not told. On exhaustion, the wake ends with `done()` forced, the session is closed normally, and `BudgetExhausted` is emitted on the agent's object. Open yields remain open and re-route at the next budget window.

An agent repeatedly exhausting budget on the same yield point is a `YieldRecurrence` signal by another name and is surfaced the same way.

## 10. Backends

The model is behind a backend interface:

```
Backend {
  name, window_tokens, supports_tools, supports_cache
  complete(system, messages, tools, max_tokens) -> {content, tool_calls, usage}
}
```

Reference backends: a local inference server on the habitat's GPU, and a remote API. Selection is per charter (an agent may be pinned to a backend) with a habitat default. Backends are ontology objects with reliability scores like modules: parse-failure rate, latency, refusal rate.

Backend swaps do not touch sessions, summaries, memory, or hooks. A session compressed under one backend continues under another; the context is rebuilt from the world anyway.

Prompt caching, where the backend supports it, is applied to the charter frame and the most recent turns. Cache stability is why the charter frame is static and first.

## 11. Sub-agent runtime

`spawn()` calls `registryd.SpawnSubAgent`, then `cortexd` runs the child as a wake with:

- context: only the object ids passed in the spawn, resolved through `AssembleContext` at depth 2
- tools: the parent's verbs intersected with the job's declared verbs
- no `remember()`, no `recall()`, no `spawn()` beyond max depth
- `done()` must return an object valid against `result_schema`; anything else is a `ResultInvalid` failure returned to the parent

The parent's wake may continue while children run, or `done()` and be re-woken by the child's return, which arrives as an observation on the `SubAgentJob`.

## 12. Human attach

A human resident may attach a conversation to an agent through the shell. This is a wake triggered by the human's session, with the human's turns entering the loop as messages attributed to the human's uid. The agent's tools are unchanged; the human gets no tools through the agent. The session is journaled like any other and linked to the human's `Session`. Attach is policy-governed: the group that may attach to an agent is a rule, same as anything else.

## 13. Journal

Every session, turn, tool call, result, hook execution, and compression is written to the agent's journald namespace with structured fields: session id, parent session, uid, trigger, backend, usage. The journal is the replay source for `ontd`'s build checks and the evidence base for every reliability score. It is append-only and retained by policy.

## 14. Failure behavior

- `AssembleContext` fails or overflows: wake does not start; `ContextOverflow` or `HookFailed` emitted on the responsible module; trigger remains open.
- Backend unavailable: wake is queued; retried with backoff; trigger remains open. Never a partial wake.
- Model output unparseable: turn is journaled as `ParseFailure`, model is re-prompted `cortexd.parse_retry` times with the parse error, then the wake ends and the trigger re-routes. Never executed, never guessed.
- `ontd` unavailable mid-wake: the wake pauses at the next tool call; resumes when available; if past a timeout, the session closes and the trigger remains open.
- Hook failure: the guarded step is rejected; the wake continues if the hook is not seed-required, ends otherwise.
- Budget exhausted: §9.
- Sub-agent TTL: job killed, `expired`, parent notified by observation.

## 15. Test obligations

- Determinism: `AssembleContext` with identical ontology state and trigger yields byte-identical context.
- Isolation: cognition cannot reach the filesystem, network, or process table except through listed tools; verified by attempting each from a test backend that emits adversarial tool calls.
- Compression: a session compressed N times retains every fact needed by a fixture task; verified by replaying recorded sessions across thresholds.
- Silence: no string derived from budget or context state appears in any model input, verified by scanning inputs in test.
- Backend swap: a session begun on backend A and continued on backend B completes the fixture task.
- Cap: private memory refuses the write past the cap and emits the observation.
- Attribution: every model call in a test run has a uid and is charged to the correct root budget, including nested sub-agents.
- Parse safety: adversarial model outputs never result in any call other than the listed tools with valid arguments.

## 16. Decisions

1. **Compression thresholds, memory cap, trust floor, idle timeout, context bound, approximate recall** are `habitat.config` values. Decided that they are configuration.
2. **Approximate recall is off** until the `ontd` search decision is made. Open.
5. **Hooks obtain cognition only by spawning a sub-agent.** No hook calls a backend directly. The on-compress summary is a sub-agent job with result schema `habitat/Summary`. Decided.
