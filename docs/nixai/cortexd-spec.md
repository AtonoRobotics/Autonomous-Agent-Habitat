# cortexd — Cognition Runtime Specification

Status: draft 2. Incorporates the 2026-09-03 review against current Anthropic agent guidance.

> Read `DESIGN.md` first. This spec is a contract: the **Guarantees** are what other services and tests depend on and must hold exactly. Mechanisms described under them are the reference approach; a builder may choose differently if every guarantee and test still holds. Thresholds and defaults are in `habitat.config` and referenced by name here; never hard-code them.

## 1. Purpose

`cortexd` is the nervous system between the agent's body (the OS, exposed through `ontd`) and its mind (a model). It owns the agent loop: waking an agent, assembling its context, running its cognition against a model backend, exposing the actions its charter and policy allow, running lifecycle hooks, compressing long sessions, enforcing cognition budgets, and closing the session into the journal.

It is a system service like any other, with its own uid, and it serves agents as clients. It never decides *what* an agent should do. It does not evaluate policy; `ontd` does. It does not run workflows; `wfd` does. It does not classify or filter model output.

Humans do not use `cortexd`. A human's runtime is their body and the shell.

## 2. Guarantees

Why: cognition is expensive and must not be wasted or hidden. Context is assembled by code before the model is called, all model calls go through the front door, and nothing about pressure is ever visible to the model.

1. An agent's cognition reaches exactly three kinds of things: ontology reads, typed observations and yields, and typed actions. The tool surface in §3 is those three kinds plus the wake's own lifecycle (`spawn`, `remember`, `recall`, `done`). No raw filesystem, process, or network access from cognition.
2. Working context is assembled to completion before the model is called. No wake proceeds on partial context.
3. No per-wake or per-turn signal derived from budget, context size, or compression state ever appears in model input. The static charter frame may state, once and identically every wake, that compression is automatic and invisible and that the agent must never stop early on account of it.
4. Compression rebuilds context from the ontology and journal. The rolling summary is one input, never the sole carrier of task state, and is typed facts, not prose.
5. Every hook is a module with a manifest, sandbox, and fixtures. A hook that needs judgment about something other than the agent's own decision obtains it by spawning a sub-agent with a result schema; it never calls a backend directly. All cognition is a wake or a sub-agent job: attributed, budgeted, journaled, and limited to the tool surface.
6. Every model call is attributed to a uid and charged to a budget. Sub-agent cognition is charged to the root ancestor.
7. Within a session, the system frame, the tool definitions, and every previously sent message are byte-frozen. New information is only ever appended. A session's backend is fixed for its lifetime; backend selection changes only at a session boundary (compression child, re-wake, sub-agent spawn).
8. Model output is never executed. It is parsed into typed tool calls with schema-valid arguments, or it is journaled as `ParseFailure`. Every tool is strict; a call that reaches the harness has valid arguments by construction.
9. Operator-authored text and world-derived text never share a channel. The charter frame and mid-session operator instructions are system content; everything derived from the world (observation snapshots, read results, interface output, human attach turns) enters as tool results or user turns and is labeled as data.
10. The raw request and response of every model call are journaled byte-for-byte.

## 3. Agent interface

The entire tool surface, exposed to the model as strict tools (`additionalProperties: false`, full `required`), each with a model-facing description generated from the manifest or type it wraps (§3.2):

```
read(query)                                  -> objects, links      # ontd read; charter-scoped contexts
context()                                    -> assembled context   # the current wake's working context, returned as a tool result
<verb>(target, args)                         -> ActionInvocationId  # one strict tool per permitted verb; see §3.1
resolve(answer, rule?)                       -> ok                  # schema generated per session from the yield's question_type
spawn(result_schema, ttl, context, task, backend?, effort?) -> SubAgentJobId
remember(fact: TypedStatement)               -> ok
recall(query)                                -> facts
done(result)                                 -> ends the wake        # schema generated per session: result_schema for sub-agents, habitat/WakeSummary otherwise
```

Forced tool choice is not used. The charter frame states the expectation ("end the wake with `done`"; "answer the yield with `resolve`"); strict schemas guarantee that any call made is valid. `tool_choice` is always automatic.

Current models emit several tool calls per assistant turn. The loop executes all calls from one assistant turn (concurrently where safe: `read`, `context`, `recall` always; verbs per their manifest's idempotency), then returns every result in a single message. A failed or rejected call is returned as an error result in the same batch, never dropped.

### 3.1 Verbs as tools

Each action the charter declares and policy permits is exposed as its own strict tool, namespaced by context (`billing_RestartService`), with its argument schema and description drawn from the action manifest. For sub-agents the set is the intersection with the job's declared verbs. Where a charter permits more verbs than the backend's tool budget comfortably carries, verbs are registered with deferred loading and a `find_verb` tool searches them; the build records which charters cross that threshold.

Rationale for per-verb tools over one `act(verb, target, args)`: each verb gets its own description, the shell renders each call typed, and strictness applies per verb rather than through a discriminated union.

### 3.2 Descriptions

Model-facing descriptions are part of the manifest (`Manifest.description`, per-argument descriptions; see `ontd` §3.4) and are required. The build rejects a description that does not state what the module does, when to use it, and when not to. Yields carry `Yield.reason`: why the workflow could not decide. Descriptions and schemas are byte-identical across wakes for a given charter version, which is what makes the tool block cacheable.

## 4. Wake lifecycle

A wake is triggered by a routed observation, a routed yield, a sub-agent job assignment, or a human attaching a conversation. Each wake is a `Session` object.

```
1. trigger arrives (dispatcher-routed)
2. pre-wake hooks run: AssembleContext and any charter-declared hooks
3. session created; backend and effort fixed for the session; parent link set if this is a compression child
4. loop:
     request = system frame + tools + messages (all prior turns verbatim, including thinking blocks)
     stream the model call; collect the final message
     branch on stop_reason:
       tool_use   -> run pre-act hooks per call; dispatch; collect results; append one result message
       end_turn   -> if done() was not called, append a system message restating the expectation, once; then end
       max_tokens -> append a system message asking for a continuation; retry once; then end as PartialTurn
       refusal    -> emit BackendRefusal; end the wake; trigger re-routes
       pause_turn -> continue
     if soft compression threshold reached and no tool round is pending: on-compress (§7), continue in child session
     if hard threshold reached: complete the pending tool round, accept no further calls, compress
5. done() or budget exhaustion or idle timeout (§9, §14)
6. post-wake hooks run (journal, memory sync, outcome recording)
7. session closed; observation/yield outcome written
```

Hook additions during a wake are appended, never merged into already-sent content: a pre-act or post-result hook's additions travel inside the tool result of the call that produced them; operator instructions mid-session are appended as system messages.

An agent that is not woken is not running. Idle agents cost nothing.

## 5. Working context

Assembled by the `habitat/AssembleContext` step before the model call. Deterministic; same inputs produce the same bytes. Sections, in order, with their channel:

| # | section | channel | authored by |
|---|---------|---------|-------------|
| 1 | Charter frame: contexts, owned types, available verbs, the crystallization obligation stated as intent, the static compression sentence (Guarantee 3) | system | operator (via charter) |
| 2 | Trigger: the observation or yield, its `question_type`, its `reason`, enumerated options | first user turn, as data | world |
| 3 | Neighborhood: `ontd.neighborhood(target, depth)` at the fixed depth in `ontd` Decision 2 | first user turn, as data | world |
| 4 | Reliability annotations for every sensor and module in the neighborhood | first user turn, as data | computed |
| 5 | Private memory: output of the retrieval step (§8); empty for sub-agents | first user turn, as data | agent |
| 6 | Rolling summary, if this is a compression child | first user turn, as data | prior session |
| 7 | Open items: unresolved yields and running sub-agent jobs; empty for sub-agents | first user turn, as data | computed |

Section 1 changes only with charter version and is the cache prefix along with the tool definitions. Sections 2 through 7 are per wake and are labeled as world-derived data; nothing in them is treated as an instruction. Directive-shaped text inside world-derived data is recorded as a `SuspiciousContent` observation on the source object and left inert.

Context size is bounded by `cortexd.context_max_tokens`. If the assembled context exceeds it, the wake fails with `ContextOverflow` on the workflow that raised the trigger. That is a bug in the workflow's yield design, not something to truncate around.

## 6. Hooks

Triggers start wakes; hooks run inside them at lifecycle points. A hook is deterministic code. Where it needs judgment about something *other than the decision the agent just made*, it calls `spawn()` and continues on the typed result. A hook never re-evaluates the agent's own decision; pre-act hooks are deterministic checks only, and a failed check returns to the agent as an error tool result in the same batch. An agent that thinks twice about one action is a wake whose context was incomplete; the fix is the context, not a checker.

| point               | inputs                     | outputs                        | channel for outputs                         |
|---------------------|----------------------------|--------------------------------|---------------------------------------------|
| pre-wake            | trigger                    | context additions              | appended to the first user turn             |
| pre-act             | pending call, context      | allow / reject with reason     | rejection as an error tool result           |
| post-result         | action result              | context additions, memory writes | inside that call's tool result            |
| on-yield-resolved   | yield, answer, rule        | crystallization candidates     | journal                                     |
| on-compress         | session, summary so far    | new `Summary` object           | next session's section 6                    |
| on-budget-threshold | remaining budget           | effort change, scope for future appends | request parameter; never model-visible |
| post-wake           | session                    | journal entries, memory sync   | journal                                     |

Seed hooks, always present: `AssembleContext`, `RecordOutcome`, `CaptureRule` (parses the stated rule against the CEL environment; an unparseable rule is `ResolveInvalid`, not a stored string), `SyncMemory`.

## 7. Sessions and compression

Sessions are lineage, not a rewritten transcript. At `cortexd.compression_soft` of the backend window or `cortexd.compression_soft_tokens`, whichever first, and only between tool rounds:

1. The on-compress hook spawns a sub-agent with result schema `habitat/Summary`, passing the prior summary and the turns since; the child returns an updated summary of typed facts.
2. The current session closes with `ended` and the summary linked.
3. A child session is created with `parent` set. It may select a different backend or effort.
4. `AssembleContext` runs again from `ontd` and the journal, with the summary as section 6.
5. The loop continues in the child.

At `cortexd.compression_hard`, the pending tool round completes, no further calls are accepted, and compression proceeds. Compaction never separates a tool call from its result.

Where a backend declares server-side compaction or tool-result clearing, the on-compress hook may prefer tool-result clearing for sessions that are long because of large results rather than many decisions; Guarantee 4 remains the default and the client-side path is always available.

Raw turns, including thinking blocks, are retained in the journal for every session.

## 8. Private memory

Each agent's private memory is a typed fact store in its home, written only through `remember()` (via `SyncMemory`) and by post-result hooks that extract facts by rule. No model summarizes the journal into memory.

```
Fact { id, statement: TypedStatement, trust: float, source: SessionId, at, superseded_by }
```

Hard cap `agents.private_memory_cap_facts`; `SyncMemory` refuses writes past it and emits `PrivateMemoryFull`. Retrieval is the `habitat/RecallFacts` step: typed lookup first, full-text second, approximate third only if `cortexd.approximate_recall`. Trust is computed by `RecordOutcome`; facts below `agents.private_memory_trust_floor` are superseded and surfaced. Sub-agents have no private memory.

## 9. Budgets and effort

`CognitionBudget` is enforced per uid. Usage counts every priced unit at the backend's declared weights (uncached input, cache write, cache read, output), converted to budget units by the backend object's price table, so a cache read costs what it costs. Sub-agent usage is charged to the root ancestor.

Effort is the first budget lever and is invisible to the model: `cortexd.effort_default` for ordinary wakes, `cortexd.effort_subagent`, `cortexd.effort_authoring` for module authoring and crystallization, `cortexd.effort_on_threshold_scope` after `budget.threshold_scope`. Charters may pin effort per wake class; backends declare supported levels. On `budget.threshold_warn` the hook lowers effort; only after that may it narrow what is appended from then on. It never edits sent content.

On exhaustion the wake ends with `done()` forced, `BudgetExhausted` is emitted, open yields remain open and re-route at the next window.

## 10. Backends

```
Backend {
  name, model_id, max_input_tokens, max_output_tokens          # discovered from the provider, not config
  capabilities: { tools, strict_tools, cache, thinking, effort_levels, streaming,
                  server_compaction, tool_result_clearing, mid_session_system, forced_tool_choice }
  prices: { input, cache_write, cache_read, output }           # per unit, for budget conversion
  stream(request) -> events; final message assembled by the adapter
}
Request  { system: [blocks], tools: [strict defs], messages: [turns verbatim], max_tokens, effort, cache_breakpoints }
Response { content: [blocks, including thinking], stop_reason, stop_details?, usage, fallback_events? }
```

Cache breakpoints: after the last charter-frame block and after the last block of the newest turn. The charter frame and tool definitions are byte-identical across wakes of a charter version.

Server-side model fallbacks are **off**. A fallback is a silent backend change inside one call, which contradicts Guarantee 6 and Guarantee 7 and hides a reliability signal. If a provider forces one, `fallback_events` is journaled as a backend switch and the session ends at the next turn boundary.

Backends are ontology objects with reliability: parse-failure rate, refusal rate, latency, `PartialTurn` rate. Selection is per charter with a habitat default; sub-agents may name a cheaper backend and lower effort at spawn.

## 11. Sub-agent runtime

`spawn()` calls `registryd.SpawnSubAgent`; `cortexd` runs the child as a wake in a disposable machine with: context limited to the passed object ids through `AssembleContext` (sections 5 and 7 empty); verbs intersected with the job's; no `remember`, `recall`, or `spawn` beyond depth; `done` whose schema is the job's `result_schema`. An invalid result is `ResultInvalid` to the parent. The parent may continue while children run, or `done()` and be re-woken by the child's return as an observation on the `SubAgentJob`.

Sub-agent tool calls are invoked on the root ancestor's behalf: policy and kernel see the root ancestor's uid; the journal records the subuid as requester. This is the decision on sub-agent identity referenced in `registryd` §7.2.

## 12. Human attach

A human resident may attach a conversation to an agent. The human's turns enter as user turns labeled as such; they are not system content and carry no authority beyond a human's presence. Attach is the one shell capability with no agent equivalent; the shell parity guarantee states the exception.

## 13. Journal

Every session, request and response body, turn, tool call, result, hook execution, compression, effort change, and backend event is written to the agent's journald namespace with structured fields. The journal is the replay source for `ontd`'s build checks, the evidence base for reliability scores, and the fixture source for offline evaluation (§15).

## 14. Failure behavior

- `AssembleContext` fails or overflows: wake does not start; `ContextOverflow` or `HookFailed`; trigger remains open.
- Backend unavailable: wake queued, retried with backoff; never a partial wake.
- `stop_reason: refusal`: `BackendRefusal` observation on the backend object with the refusal category; wake ends; trigger re-routes. A trigger refused `cortexd.refusal_reroute_max` times is routed to the owner as a yield of type `RefusedTrigger`.
- `stop_reason: max_tokens`: continuation requested once by appended system message; then `PartialTurn` and the wake ends.
- Parse failure (arguments invalid despite strict schemas, or a non-tool final turn where a tool was expected): the failed assistant turn is kept verbatim; the error is appended as an error tool result or user turn; retried `cortexd.parse_retry` times; then the wake ends.
- `ontd` unavailable mid-wake: pause at the next tool round; resume; past `cortexd.wake_idle_timeout` the session closes and the trigger remains open.
- Idle: no streamed event for `cortexd.wake_idle_timeout`, measured on the stream, not on completed turns.
- Budget exhausted: §9. Sub-agent TTL: job killed, `expired`, parent notified.

## 15. Test obligations

- Determinism: `AssembleContext` with identical ontology state and trigger yields byte-identical context.
- Isolation: cognition cannot reach the filesystem, network, or process table except through listed tools; verified with a test backend emitting adversarial calls.
- Append-only: for every consecutive pair of requests in a session, the earlier request body is a byte-prefix of the later one up to the newly appended turns. Run in CI against a backend that rejects prefix mismatches.
- Strictness: every tool definition sent has `additionalProperties: false` and a full `required` list; a fuzzer cannot produce a call that reaches the harness with invalid arguments.
- Batching: an assistant turn with N tool calls produces exactly one result message with N results; a rejected call is an error result in that message.
- Compression: a session compressed N times retains every fact needed by a fixture task; the compression child's backend may differ from the parent's.
- Silence: no string derived from budget, context size, or compression state appears in any model input except the constant charter-frame sentence.
- Attribution: every model call in a test run has a uid and is charged at the backend's weights to the correct root budget, including nested sub-agents.
- Refusal: a refused request produces `BackendRefusal`, ends the wake, and leaves the trigger open.
- Offline evaluation: a fixture set of recorded wakes (context, trigger, expected disposition) replayed against a backend; a change to the charter frame, tool descriptions, effort defaults, or backend must not regress the pass rate below `cortexd.eval_min_pass`.

## 16. Decisions

1. **Thresholds, caps, timeouts, effort defaults, and context bounds** are `habitat.config` values.
2. **Approximate recall is off** until the `ontd` search decision is made. Open.
3. **Hooks obtain cognition only by spawning a sub-agent.** Decided.
4. **Per-verb strict tools, deferred loading past the tool budget.** Decided.
5. **No forced tool choice; expectations stated in the charter frame.** Decided.
6. **Backend fixed per session; server-side fallbacks off.** Decided.
7. **Sub-agent calls are policy- and kernel-evaluated as the root ancestor; the subuid is the recorded requester.** Decided.
8. **Effort before scope as the budget lever.** Decided.
