# Review: "The Habitat" spec bundle (NixAI_2) against Anthropic's current agent guidelines

Reviewed: `docs/nixai/` (DESIGN.md, ontd-spec.md, wfd-spec.md, cortexd-spec.md, registryd-spec.md, habitat-shell-spec.md, os-build-spec.md, habitat.config), all "draft 1" except os-build (draft 2). Copied verbatim from the uploaded `NixAI_2.zip` so the line references below resolve.

Also consulted: the `AtonoRobotics/Nix-AI` repository (v2.1.0 contract, `crates/habitat-models`, `crates/habitat-harnesses`) to see how the bundle relates to what is already built.

Guidelines used as the yardstick (Anthropic, as of 2026-09):

- Claude API docs: tool use (define tools, how tool use works, strict tool use, parallel tool use), prompt caching, adaptive thinking and effort, compaction and context editing, preserved thinking on Claude Fable 5.1, refusal stop reason and server-side fallbacks, mid-conversation system messages.
- Prompting best practices for current models: agentic systems (long-horizon state tracking, multi-window workflows, subagent orchestration, autonomy and safety, overeagerness).
- Anthropic engineering guidance on building effective agents, context engineering, writing tools for agents, and long-running agent harnesses. `www.anthropic.com` was blocked by the session's egress policy, so those are cited from the bundled reference material, not fetched live.

## 1. Verdict

The architecture is in unusually good shape against Anthropic's guidance. The core moves are exactly what the guidance now recommends: deterministic context assembly before the model call, a small typed tool surface, model output parsed into typed calls and never executed, fresh-context sub-agents with a result schema, compaction that rebuilds from durable state rather than trimming the transcript, a cache-stable static prefix, and an explicit memory surface. Several things the guidance warns about are already designed out (regex-parsing decisions out of prose, hidden model calls, pressure warnings shown to the model).

The gaps are concentrated in one place: `cortexd-spec.md` describes the model boundary at a level of abstraction that predates the current API. As written, the backend contract cannot be implemented correctly on Claude Fable 5.1 or Claude Opus 5, and three guarantees (7, 3, and the parse-retry path) are in tension with API behavior that is now enforced rather than advisory. None of this changes the design thesis. It does change what the backend interface, the wake loop, and several test obligations must say.

Severity legend: **Blocking** means an implementation following the spec as written will fail or violate a guarantee on the current API. **Should fix** means the spec allows an implementation that contradicts the guidance. **Consider** is an improvement the guidance recommends that the spec is silent on.

## 2. Where the design already matches the guidance

| Guidance | Where the bundle does it |
|---|---|
| "If you're writing a regex to extract a decision from model output, that decision should have been a tool call." | cortexd Guarantee 8 and §14: output parsed into typed calls or journaled as `ParseFailure`; `done`, `resolve`, `act` are tools. |
| Context is a finite resource; assemble the minimum high-signal set; stable content first for caching. | cortexd §5: ordered context, static charter frame first, bounded neighborhood, `ContextOverflow` instead of truncation. |
| Simple compaction: summarize into one message, start the next request from the summary plus fresh state, replay nothing else. | cortexd §7: child session, context rebuilt from ontology and journal, summary as one section. This is the shape the preserved-thinking guidance recommends and the two shapes it warns about (keep-tail, background swap) are avoided. |
| Sub-agents with a clear objective, output format, and boundaries; fresh context; async where possible. | cortexd §11, registryd §3.5 and §4.5: result schema declared before spawn, TTL, verb intersection, parent may continue while children run. |
| Give the model a memory surface and tell it where and how to use it. | cortexd §8: `remember` / `recall`, trust scoring, hard cap that forces crystallization. |
| Do not show the model a remaining-token countdown. | cortexd Guarantee 3 and the "Silence" test obligation. |
| Treat external content as data, never as instructions. | Nix-AI W07 already models `UntrustedExternalData`; the bundle's typed-observation model implies it but does not state it (see F9). |
| Tool descriptions and gating: promote actions that need gating, auditing, or rendering to dedicated typed tools. | The whole `act(verb, target, args)` path with policy at `ontd.act`. |
| Keep the fixed tool set small and consolidated. | Eight tools in cortexd §3. |

## 3. Findings

### F1. Blocking: the backend interface cannot express the current API

`docs/nixai/cortexd-spec.md:161-165` defines `Backend { name, window_tokens, supports_tools, supports_cache; complete(system, messages, tools, max_tokens) -> {content, tool_calls, usage} }`.

What is missing, each of which the wake loop depends on:

- **`stop_reason`.** The loop in §4 has no branch for `refusal`, `max_tokens`, or `pause_turn`. On Claude Fable 5.1 a refused request returns HTTP 200 with `stop_reason: "refusal"` and empty or partial content; the guidance is to branch on `stop_reason` before reading content. §10 tracks "refusal rate" as a backend reliability score but nothing in the loop produces that signal. The `os` context (editing units, network config, kernel updates) is exactly the kind of content that can trip the cyber classifier, so this is not theoretical.
- **Thinking blocks.** Current models return `thinking` blocks that must be replayed verbatim on the next call of the same session, and on Claude Fable 5.1 a thinking block's signature is bound to the exact prefix (system, tools, all prior messages). `{content, tool_calls}` loses them. The session record (§13) must store the full assistant content array, not a projection of it.
- **Effort.** `output_config.effort` is the primary cost and depth lever on every current model and is invisible to the model. The spec has no place for it. See F6.
- **Streaming.** Single turns on Claude Fable 5.1 at high effort can run many minutes; the SDKs require streaming for large `max_tokens`. A blocking `complete()` with `wake_idle_timeout = 10m` (`habitat.config:29`) will time out healthy wakes.
- **Tool result semantics.** No `is_error`, no requirement that all results for one assistant turn go back in a single user message. See F5.
- **Caching markers.** `supports_cache` is a boolean; the spec needs to say where breakpoints go (last charter-frame block; last content block of the newest turn) and that the charter frame must be byte-identical across wakes of the same charter version.

Recommended contract shape (names are the builder's):

```
Backend {
  name, model_id, max_input_tokens, max_output_tokens      # discover via the Models API, not config
  capabilities: { tools, strict_tools, cache, thinking, effort_levels, streaming,
                  server_compaction, mid_conversation_system, forced_tool_choice }
  stream(request) -> events                                # final message via the SDK's final-message helper
}
Request  { system: [blocks], tools: [defs], messages: [turns], max_tokens, effort, cache_breakpoints }
Response { content: [blocks incl. thinking], stop_reason, stop_details?, usage, fallback_events? }
```

Journal the raw request and response bodies (§13 already stores context byte-for-byte; extend that to the whole request so the preserved-thinking check in F2 is testable).

### F2. Blocking: the session transcript must be append-only, and the spec permits edits

Claude Fable 5.1 binds each thinking block to the conversation prefix that produced it. Editing an earlier turn, rebuilding `system` or `tools` between requests of one session, or injecting and then removing per-turn text invalidates every later thinking block. This is enforced with a 400 for accounts created on or after 2026-08-31 and planned for all accounts on later models. It also invalidates the prompt cache from the edit point on any model.

Places the spec allows or implies an edit inside one session:

- `cortexd-spec.md:93-101`: pre-wake and post-result hooks return "context additions". If these are merged into the charter frame or into section 2 through 7 of the already-sent context, that is an edit. They must arrive as appended content: inside the `tool_result` of the call that produced them, or as an appended `role: "system"` message.
- `cortexd-spec.md:153`: on budget threshold the hook "may narrow scope: reduce neighborhood annotations". If that rewrites context already in the transcript, it is an edit. Narrowing may only affect what is appended from then on, or the next child session. Lowering `effort` is the sanctioned lever and does not touch the prefix (F6).
- `cortexd-spec.md:197`: on parse failure the model is "re-prompted with the parse error". This is fine only if the parse error is appended as a new turn (a `tool_result` with `is_error: true`, or a user turn) and the failed assistant turn is kept verbatim. Say so.
- `cortexd-spec.md:35`: `context()` is "re-readable". Returning the assembled context as a tool result is fine; regenerating the system frame is not.
- `cortexd-spec.md:122`: the hard compression threshold "forces compression regardless of turn boundaries". Compaction must not split an assistant `tool_use` from its `tool_result`. Change to: at the hard threshold, the pending tool round completes and no further tool calls are accepted until the child session starts.

Add a guarantee: *Within a session, `system`, `tools`, and every previously sent message are byte-frozen; new information is only ever appended.* Add a test obligation: *For every consecutive pair of requests in a session, the earlier request body is a byte-prefix of the later one up to the newly appended turns* (this is step 1 of Anthropic's three-step check). In CI run against `claude-fable-5-1` with `thinking.block_binding.prefix_mismatch_behavior: "error"` under the `thinking-binding-controls-2026-08-01` beta so any edit fails the run.

### F3. Blocking: Guarantee 7 and the "Backend swap" test are stronger than the API allows

`cortexd-spec.md:26` says the backend is replaceable "with zero change to any ... session record", and `cortexd-spec.md:209` tests "a session begun on backend A and continued on backend B". Thinking blocks are bound to the producing model. Switching models mid-session silently drops them (unbilled) on most models and Claude Fable 5.1 reads only its own. It also resets the prompt cache, which is model-scoped.

`cortexd-spec.md:170` already has the right idea: "a session compressed under one backend continues under another." Make that the guarantee. Backend selection is fixed for the lifetime of a session and may change only at a session boundary (compression child, re-wake). Reword the test to "a wake begun on backend A whose compression child runs on backend B completes the fixture task."

One consequence for headcount and budget: a sub-agent may run on a cheaper backend at lower effort (this is the sanctioned way to use a cheaper model without breaking the parent's cache), so `spawn()` or the charter should be able to name the sub-agent backend and effort.

### F4. Blocking: forced tool use is not available, so `done()` and `resolve()` need strict schemas

Claude Fable 5.1 rejects `tool_choice: any` and `tool_choice: tool` with a 400. The spec relies on the model ending a sub-agent with a `done()` whose argument validates against `result_schema` (`cortexd-spec.md:181`) and on `resolve()` answers validating against `question_type`. The harness cannot force those calls. What it can do:

- Mark every tool `strict: true` with `additionalProperties: false` and a full `required` list, so any call that is made has schema-valid arguments. This removes most of the `ParseFailure` path (`parse_retry = 1` in `habitat.config:33` becomes a rare fallback).
- Generate the `done` tool's `input_schema` per session from `result_schema` and the `resolve` tool's from the yield's `question_type`, then keep it fixed for the session.
- State the expectation in the charter frame ("end the wake with `done`"; "answer the yield with `resolve`") rather than in `tool_choice`.

The Nix-AI W09 adapter (`crates/habitat-models/src/lib.rs:254-273`) already parses a `submit_disposition` tool call; whichever design proceeds, the `tool_choice` used to elicit that call must be `auto`.

### F5. Should fix: parallel tool calls and result batching are unspecified

`cortexd-spec.md:56-60` processes calls one at a time and "appends turn". Current models emit several `tool_use` blocks per assistant turn by default. The guidance: execute them (concurrently where safe, and `read`, `context`, `recall` are safe), then return **all** `tool_result` blocks in **one** user message; splitting them across messages trains the model to stop parallelizing. A failed call returns `tool_result` with `is_error: true`, never a dropped result. Spec the loop as: one assistant turn, a set of results, one user message. Pre-act hooks run per call; their rejections are `is_error` results in the same batch.

### F6. Should fix: effort is the missing budget lever

The on-budget-threshold hook narrows what the model sees (`cortexd-spec.md:153`), which risks F2 and degrades decisions. Anthropic's guidance ranks the levers as: caching first, then `output_config.effort` (invisible to the model, no prefix change, no cache reset on Claude Fable 5.1 and Claude Opus 5 when sent as a per-message effort system message under `mid-conversation-output-config-2026-07-01`), then model choice via sub-agents. Recommended:

- `habitat.config [cortexd]`: `effort_default = high`, `effort_subagent = low`, `effort_on_threshold_scope = medium`, `effort_authoring = xhigh` (module authoring and crystallization are the hardest wakes and the guidance says to spend effort there).
- Charters may pin effort per wake class. Backends declare supported levels.
- The hook lowers effort before it narrows context.

This is consistent with Guarantee 3: effort is a request parameter, not a string the model sees.

### F7. Should fix: Guarantee 3 bans a statement the guidance recommends

Guarantee 3 (`cortexd-spec.md:22`) says nothing about compression is "ever visible to the model". The guidance agrees on the dynamic part (no countdowns, no pressure warnings), and separately recommends telling the model once, statically, that context will be compacted automatically so it never stops early or suggests a new session on its own, and that it should never stop tasks early on account of context. Claude Fable 5.1 shows occasional "context anxiety" in very long sessions without this.

Suggested wording: *No per-wake or per-turn signal derived from budget, context size, or compression state ever appears in model input. The static charter frame may state, once, that compression is automatic and invisible and that the agent must never stop early because of it.* The Silence test (`cortexd-spec.md:208`) still holds because the sentence is constant.

### F8. Should fix: refusal handling and server-side fallbacks

Nothing in §14 handles `stop_reason: "refusal"`. Two decisions are needed:

- **Whether to opt into server-side fallbacks** (`fallbacks: "default"` under `server-side-fallback-2026-07-01`, or an explicit model list). Anthropic's default recommendation for Claude Fable 5.1 and Claude Opus 5 code is yes. In this design a fallback is a silent backend change inside one call, which conflicts with attribution (Guarantee 6) and with backends being ontology objects with their own reliability scores. If opted in, the `fallback` content block must be journaled as a backend switch and the session continues on the fallback backend for its remaining life (F3).
- **What a refusal is in the ontology.** Recommend: a `BackendRefusal` observation on the backend object carrying `stop_details.category`, the wake ends, and the trigger re-routes. Refusal rate then feeds the existing reliability score. A trigger refused N times is a `YieldRecurrence`-class signal for the owner.

Also note: Claude Fable 5.1 is not served under zero data retention; an organization on ZDR gets a 400. The journal already retains everything so this is a deployment fact to record, not a design problem.

### F9. Should fix: the operator channel and untrusted content are not distinguished

Three kinds of text reach the model: the charter frame (operator authority), hook additions and human attach turns (`cortexd-spec.md:187`), and world-derived strings (observation `properties_at` snapshots, `read()` results, interface-action output). The guidance:

- Operator instructions added mid-session go in an appended `{"role": "system"}` message, never as text inside a user turn (that is the spoofable channel).
- World-derived content is data. It should enter only as tool results, be labeled as such, and never be placed in the system frame.

Nix-AI W07 already has this (`UntrustedExternalData`, directive-shaped payloads become a security observation and stay inert). Port the rule into cortexd §5 and §6: which context sections are operator-authored and which are world-derived, and the channel each uses.

### F10. Should fix: tool descriptions and per-verb argument schemas are unspecified

Anthropic: detailed descriptions are "by far the most important factor" in tool performance (what, when, when not, caveats; three or more sentences), use `enum` for fixed sets, `strict: true`, `input_examples` for complex inputs. The spec never says where a verb's model-facing description comes from. `Manifest` (`ontd-spec.md:82-98`) has no `description` field; `Action.args: [{name, type}]` has no per-argument descriptions; `Yield.question_type` is a `TypeId` with no explanation of what the question is.

Recommend: `Manifest.description` (required, model-facing, checked by a build rule for minimum length and for naming the when/when-not conditions), per-argument `description`, and `Yield.reason` (why the workflow could not decide). Then decide how verbs are presented:

- Keep the single `act(verb, target, args)` tool and render the permitted verbs' schemas as a `oneOf` discriminated on `verb` inside a strict schema. Cheapest for the tool count, consistent with "consolidate related operations into one tool with an action parameter".
- Or one strict tool per permitted verb, namespaced by context (`billing_RestartService`), with `defer_loading` and tool search when a charter permits many verbs. This is the guidance's answer for large tool libraries and gives the shell a typed rendering of each call.

Either is acceptable; the spec should pick one and make the build check its descriptions.

### F11. Should fix: time budgets do not fit current turn lengths

`wake_idle_timeout = 10m`, `subagent_default_ttl = 30m`, `machine_default_ttl = 30m`, `step_timeout_default = 5m` (`habitat.config:29,18,40,37`). A single Claude Fable 5.1 request at `high` or `xhigh` can take 15 minutes. Define idle as "no streamed event for N minutes" rather than "no completed turn", and size sub-agent TTLs from expected turns times effort, not a flat default. Also `compression_soft = 0.50` of the window means a 500K-token session on a 1M model; that is allowed, but each turn resends the whole prefix, so cost per wake grows quadratically. Consider an absolute `compression_soft_tokens` alongside the ratio.

### F12. Consider: server-side compaction and context editing as backend capabilities

The bundle's client-side compaction is the recommended shape. Server-side compaction (`compact-2026-01-12`) and tool-result clearing (`context-management-2025-06-27`) are alternatives that do not count as transcript edits and keep the cache. Guarantee 4 (rebuild from the world) should stay the default, but the backend capability list (F1) can expose these and the on-compress hook can prefer tool-result clearing for sessions that are long because of large results rather than long because of many decisions.

### F13. Consider: evaluation of cognition quality

The test obligations are deterministic and thorough, and reliability scores are an online evaluation. What is missing is an offline eval: a fixture set of recorded wakes (context, trigger, expected disposition) replayed against a backend to detect regressions when the charter frame, tool descriptions, effort defaults, or the model change. The journal already stores byte-identical context, so this is cheap to add, and the guidance treats prompt changes without an eval as unmeasured.

### F14. Consider: de-prescription and intent in the charter frame

The guidance for Claude Fable 5.1: prompts written for older models are often too prescriptive and reduce quality; state the goal and constraints rather than the steps; give the reason behind a request. The charter frame is already at the right altitude (contexts, owned types, verbs). Two additions worth making explicit: the yield carries *why* the workflow could not decide (F10's `Yield.reason`), and the frame includes the crystallization obligation as intent ("when you are asked the same class of question twice, the answer is a step") rather than as a procedure.

## 4. Spec contract defects (independent of Anthropic guidance)

These are internal consistency problems found while reading the bundle as a contract.

1. **Decision numbering gaps.** cortexd §16 lists 1, 2, 5; registryd §11 lists 1, 4. Either decisions were removed without renumbering or they were never written.
2. **`habitat.config` says a key is not configuration.** `neighborhood_depth = 2  # fixed; not tunable` (`habitat.config:31`) contradicts DESIGN.md's rule that config values are configuration. Either it is a spec constant (move it to ontd Decision 2 and delete the key) or it is tunable.
3. **Budget totals are half-specified.** `Headcount.budget_total: CognitionBudget` (`registryd-spec.md:67`) has tokens and GPU-seconds, but config has only `budget_total_tokens` (`habitat.config:8`). Also unspecified: which usage fields count against `tokens_per_day` (input, output, cache read, cache write are priced differently, and cache reads on Claude Fable 5.1 cost a fraction of input).
4. **Machine image naming.** os-build calls it `os/Image` (`os-build-spec.md:92,121`); registryd and wfd call it `os/Configuration` of kind `machine` (`registryd-spec.md:49`). One name.
5. **`ApprovalRequest` contradicts the design.** habitat-shell §4.3 (`habitat-shell-spec.md:80`) mentions "`ApprovalRequest`-class questions". DESIGN.md and ontd §3.8 say there is no approval concept, only `Cosign` objects created by policy rules. Delete the term or define it as a `Cosign` yield.
6. **`InterfaceGap` is referenced but never defined.** DESIGN.md, os-build §11, and habitat-shell §7 use it; no spec lists it as a type, sensor, or observation, and ontd Guarantee 1 would reject an unreferenced type.
7. **Sub-agent identity versus policy.** Actions run as the invoker's uid (wfd §6), sub-agents run as a subuid from the root's range (registryd §7.2), and policy is evaluated on the caller's group membership (ontd §3.8). Nothing says that a subuid inherits the root ancestor's groups for policy evaluation, or that it does not. The sub-agent containment test (`registryd-spec.md:260`) needs this decided.
8. **Charter versus policy wording.** registryd §4.1 and §4.2 preconditions say "Caller's charter includes `CreateAgent`" (`registryd-spec.md:131,154`), while registryd Decision 4 and ontd §3.8 say authorization is policy, not charter. Reword to "policy permits the caller to invoke `CreateAgent`"; charter `invokes` is declared use, and a mismatch is already a build failure.
9. **Sub-agent context sections.** cortexd §11 says sub-agent context is "resolved through `AssembleContext` at depth 2" while §8 says sub-agents have no private memory. State that sections 5 (private memory) and 7 (open items) are empty for sub-agents so the determinism test has a defined expected output.
10. **Model-authored rules are code.** `resolve(YieldId, answer, rule?)` accepts a CEL predicate authored by the model (`ontd-spec.md:200`). `CaptureRule` stores it; nothing parses or sandboxes it before storage. Add: the rule is parsed against the CEL environment of ontd Decision 1 at resolve time, and an unparseable rule is a `ResolveInvalid` on the yield, not a stored string.
11. **Shell parity has an unstated exception.** habitat-shell Guarantee 4 and the parity test say anything a human can do, an agent can do through cortexd. Attach (§4.2) has no agent equivalent. State the exception.
12. **"Three calls" versus eight tools.** DESIGN.md says the agent's interface is three calls; cortexd §3 lists eight tools in four groups (read, observe/yield, act, and spawn/memory/done). Harmless, but a contract should not disagree with its rationale document about the size of its surface.
13. **Machine boot timing.** os-build §7 and §12 require boot under one second; `wfd.machine_boot_timeout = 5s`. Fine as a timeout, but say which one the image test asserts.

## 5. Relation to the Nix-AI repository

The bundle is not a revision of the v2.1.0 contract in `AtonoRobotics/Nix-AI`; it is a different design with a different vocabulary (ontology, sensors, yields, crystallization, headcount) versus v2's (objectives, wakes, activations, effects, capability packages, system generations). Some v2 work is directly reusable and in two cases already ahead of the bundle:

- `habitat-context` (W07) already implements truth classes, provenance, freshness, budget-driven omission with recorded uncertainty, and the untrusted-external-data rule (F9). The bundle's `AssembleContext` should be specified as a profile of it rather than a replacement.
- `habitat-effects` (W08) implements durable effect admission, attempts, observation, and reconciliation. The bundle's action path (`ontd.act` to `wfd` to result object) has no reconciliation story for actions whose external effect is ambiguous after a crash; wfd §10 "Host restart" says results are re-checked, which is the W08 problem restated. Reuse it.
- `habitat-models` (W09) is provider-neutral and translates only a `submit_disposition` tool call. It carries none of the API-shape concerns in F1 because it never builds the request. Whichever request builder is written for the bundle (or for v2) must address F1 through F5; the adapter's `tool_choice` must be `auto` (F4).
- Nix-AI's own `AGENTS.md` "outcome integrity" rule and its fresh-agent review rule are compatible with DESIGN.md's "correct beats done"; nothing in the bundle contradicts them.

If the intent is for the bundle to supersede v2.1.0, the decision register (`contracts/architecture/14-DECISION-REGISTER.md`) is where that has to be recorded, with the retained modules named.

## 6. Recommended edits, in priority order

1. cortexd §10: replace the backend interface with the capability-declaring, streaming, full-content shape in F1; add `stop_reason` handling to §4 and §14.
2. cortexd §2: add the append-only guarantee (F2); rewrite Guarantee 7 and the backend-swap test to session boundaries (F3); refine Guarantee 3 per F7.
3. cortexd §3 and §11: strict schemas on every tool, per-session generated `done` and `resolve` schemas, `tool_choice: auto` only (F4).
4. cortexd §4: one assistant turn, one batched result message, `is_error` for rejections (F5).
5. cortexd §9 and `habitat.config`: effort as the first budget lever (F6).
6. cortexd §14: refusal as a typed observation; decide fallbacks (F8).
7. cortexd §5 and §6: operator channel versus world-derived data (F9).
8. ontd §3.4, §3.7, §3.9: model-facing descriptions, argument descriptions, yield reason, build check (F10).
9. `habitat.config`: timeouts and TTLs sized for multi-minute turns (F11).
10. Section 4 items 1 through 13.

Items 1 through 4 should land before any implementation of cortexd starts; the rest can follow the first fixture wake.
