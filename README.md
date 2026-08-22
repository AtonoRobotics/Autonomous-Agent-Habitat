# Autonomous Multi-Agent Habitat (AMH)

A continuously operating habitat for autonomous agents. It wakes from goals, schedules, events, and messages; supplies durable work, context, memory, tools, workers, and recovery; and exposes stable seams on which domain applications are built.

AMH is not a governance product, a security product, a physical-device stack, or a domain application. Small hard core, reversible extensions: the core contains only the invariants every domain requires. Models, harness policies, tools, memory implementations, user surfaces, and domain behavior attach as replaceable, individually installable, individually removable extensions — see `docs/AMH-SPECIFICATION.md` §1 for the full governing decision set. Physical-device actuation in particular is not core at all; it belongs to a separate Physical AI extension, designed and built independently.

## Layout

Only directories that hold real, working code are listed — nothing here is a reserved name for future work.

```
daemon/                Go control-plane daemon (single binary)
├── cmd/amh-daemon              supervisor entrypoint (systemd/Windows Service)
├── supervisor/                 OTP-style supervision tree
├── scheduler/                  interval ticker for routines
├── health/                     healthz endpoint, watchdog
├── authn/                      bearer-token RBAC (agent/operator roles)
├── api/                        HTTP admin surface: extensions, computers, accounts,
│                                model-provider inference
├── mcp/                        MCP server: exposes AMH capabilities as MCP tools
│                                over Streamable HTTP
├── a2a/                        A2A 1.0 adapter: exposes AMH's goal-pursuit skill
│                                to external A2A clients over HTTP+JSON
├── extensions/                 Cordis-lifecycle extension registry: discover,
│                                activate, quiesce, dispose, dependency resolution
├── sandbox/                    per-agent computer provisioning (container or
│                                Linux namespace isolation)
├── credentials/                AES-256-GCM encrypted account/module credential store
├── inference/                  model-provider seam: agents get real inference through
│                                the daemon using only their agent token, never a
│                                model-provider credential of their own
├── policy/                     generic policy/approval seam: fail-closed action
│                                admission, bound to an exact action digest
├── selfimprove/                self-improvement candidate lifecycle: generate,
│                                eval, canary, promote, demote, rollback
├── operations/                 generic external-effect lifecycle: propose, dispatch,
│                                observe, reconcile — crash recovery for interrupted
│                                effects (§4, acceptance invariant #2)
├── backup/                     pg_dump/pg_restore-backed backup and restore
├── observability/              OpenTelemetry tracing
└── store/                      PostgreSQL open + migration runner

agents/                 Python cognition layer (uv-managed)
├── harness/                    tool-calling loop, scoped VFS, planning, MCP client
├── context/                    token budget manager, compaction
├── memory/                     working/episodic/semantic/entity/procedural projections
└── workflows/                  DBOS durable workflows: goal pursuit and
                                 control-plane HTTP clients

extensions/             Installed extensions, activated through daemon/extensions
└── control-plane-ui/           first-party operator UI, itself an extension —
                                 not baked into the daemon

contracts/              Public interface contracts
├── ontology.schema.json
├── envelope.schema.json
├── extension-manifest.schema.json
├── action-envelope.schema.json
├── effect-record.schema.json
├── policy-decision.schema.json
├── manifests/                  example agent/skill manifests
└── proto/                      reserved for a future gRPC daemon<->agent transport

store/migrations/       Canonical SQL DDL, applied by daemon/store and agents/workflows/ontology.py
└── down/                       paired reverse migration for each *.sql above, used by
                                 daemon/store.Rollback and `amh-daemon -rollback-migration`
fixtures/                a pinned MCP server, test-only key material
```

## What's real right now

Everything below is working code with tests that exercise it against real processes, a real PostgreSQL instance, and (where applicable) a real network protocol — not mocked.

- **Durable execution.** DBOS Transact on PostgreSQL, the sole authoritative store from day one. A crashed process resumes exactly where it left off, no duplicate committed steps — proven by restart-mid-flight tests on the goal-pursuit workflow.
- **Extension registry (`daemon/extensions`).** Discover, activate, quiesce, dispose against `contracts/extension-manifest.schema.json`; dependency resolution between capability providers and consumers; disposal recorded as activation's verified inverse. Knowledge base, memory, model providers, and harnesses are all just extensions to this registry — it has no domain-specific logic.
- **Computers (`daemon/sandbox`).** Each agent's own isolated compute instance: container-backed via docker, or process-backed via a real Linux mount namespace when no docker daemon is present. Create/Destroy is a verified-inverse pair.
- **Credentials (`daemon/credentials`).** AES-256-GCM encrypted at rest, fails closed with no encryption key configured, rotation-aware, never returns a secret over the admin API.
- **Control-plane UI (`extensions/control-plane-ui`).** Installs harnesses, provisions computers, and authenticates accounts — installed and activated through the extension registry itself, not compiled into the daemon.
- **Authorization.** Two bearer-token roles (agent, operator), constant-time comparison, fail-closed daemon startup if either token is unset. An agent token is mechanically refused on every operator-only route (extension install/activate/dispose, credential writes) — not a convention, enforced by the server.
- **Context management.** Per-tool-result token cap, budget tracking, compaction triggered at a configurable threshold, trace-context propagation across DBOS worker-thread boundaries so a subordinate agent's span nests under its parent's.
- **MCP client.** stdio transport, tested against a real third-party MCP server (pinned, not `npx`-refetched per run).
- **Model-provider inference (`daemon/inference`).** Agents call `POST /v1/inference/{complete,count-tokens}` on the daemon using only their agent bearer token; the daemon holds the actual provider credential (registered once as a `daemon/credentials` account) and makes the real call. An `anthropic` kind speaks the native Messages API (API key or OAuth bearer); an `openai_compatible` kind speaks chat completions — between the two, Anthropic, OpenAI, Grok/SuperGrok, GLM/Z.ai's Coding Plan, and self-hosted vLLM/Ollama are all covered without per-vendor code. OAuth subscription tokens refresh and rotate back into the credential store automatically. `agents/context/llm.py` is a pure HTTP client to this seam — it holds no model-provider credential of its own. `agents/context/budget.py`'s token counting is a real tokenizer, not a `chars // 4` approximation. Every `Complete`/`CountTokens`/`Embed` provider attempt is now the first real call site for `daemon/operations` (§4): each is proposed, marked dispatch-pending/dispatched, then resolved confirmed or failed from the real response — a model-provider call declares reversibility `verified` because it has no side effect outside its own response, the same "reversible by construction" category `daemon/sandbox`'s Create/Destroy pair uses. `Router.Operations` is nil-safe (untracked, same soft-disable posture as `Server.Credentials`) for callers that don't wire it; `daemon/api.New` always does.
- **Generic policy and approval seam (`daemon/policy`).** The core's one domain-neutral policy: admit iff a proposed action declares a verified, attested reversibility; everything else needs an operator's Approve/Deny. A `PolicyDecision` is bound to an exact action digest (recomputed server-side from the payload, never trusted from the caller) and single-use — `Consume` atomically fails closed on expiry, a digest mismatch, or reuse. Approving a `needs_approval` decision mints a fresh, freshly-bound decision rather than mutating the original, so every decision's decided-at/expiry stays internally consistent. An agent token is mechanically refused on Approve/Deny — the same anti-self-approval property as every other operator-only route.
- **A2A 1.0 adapter (`daemon/a2a`).** External A2A clients discover this habitat via an unauthenticated `/.well-known/agent-card.json`, then call `message:send`/`GetTask`/`ListTasks`/`CancelTask` over the HTTP+JSON binding (the one A2A explicitly supports whose method/path mapping is unambiguous straight from the protocol's own proto definitions, unlike the JSON-RPC binding's method-name convention). `SendMessage` durably creates a real AMH Goal — see "What's declared but not yet built" for the one thing it doesn't yet do.
- **Signed extension packs and compatibility qualification (`daemon/extensions`).** `Discover` unconditionally checks a manifest's `spec.compatibility.amhCore` semver range against this daemon build's own `CoreVersion` — an incompatible manifest is refused outright, not merely warned about. If a manifest declares `spec.signature`, `Discover` verifies it for real: the declared digest must equal the manifest's own server-recomputed `SignableDigest` (the same "never trust a caller-supplied digest" property `daemon/policy` enforces for action digests), the `keyId` must resolve to a currently-trusted, non-revoked Ed25519 key in the new `trusted_signing_key` store (`POST/GET /v1/extensions/trusted-keys`, `POST /v1/extensions/trusted-keys/{keyID}/revoke`, all operator-managed), and the signature itself must verify against that key. Real Ed25519 keys, real `ed25519.Verify` calls, real rejection of a tampered manifest, an unknown key, or a revoked key — proven by tests that sign with `crypto/ed25519`, not mocked.
- **Backup and restore (`daemon/backup`).** `POST /v1/backup` streams a real `pg_dump` custom-format snapshot of the daemon's own PostgreSQL store; `POST /v1/restore` applies one back via `pg_restore --clean --if-exists --single-transaction` — atomic: either the whole restore commits or the target is left exactly as it was. Both are operator-only. Proven by a real round-trip test: seed a row, back up, mutate further, restore, and confirm the post-backup mutation is gone while the pre-backup row is back.
- **Migration rollback (`daemon/store`).** Every migration under `store/migrations/*.sql` now has a paired reverse migration under `store/migrations/down/` (same filename). `store.Rollback` finds the most recently applied migration from `schema_migrations`, requires its down-file to exist — failing closed with `ErrNoDownMigration` rather than silently no-op'ing or reversing the wrong one — and applies it in a transaction. `amh-daemon -rollback-migration=N` runs it N times and exits, without starting the daemon; it's a maintenance operation against a stopped daemon, not a live admin-API route, since rolling back a schema out from under an actively-serving process is a different, riskier thing than backup/restore is. Proven by tests that roll every currently-defined migration all the way back to an empty schema, then re-apply forward cleanly.
- **External-effect lifecycle (`daemon/operations`).** §4's generic operation state machine — `PROPOSED -> ADMITTED|REJECTED|NEEDS_APPROVAL -> DISPATCH_PENDING -> DISPATCHED -> OBSERVED|OUTCOME_UNKNOWN -> CONFIRMED|RECONCILED|COMPENSATED|FAILED` — as real, durable, tested mechanics, not just a diagram in the spec. `Propose` composes with `daemon/policy` for admission rather than reimplementing it; `MarkDispatchPending` consumes the admitting `PolicyDecision` atomically, the caller's durable commitment to actually attempt the external call next. `ReconcileInterrupted` runs once at daemon startup, before anything else touches operations: it finds every effect still `dispatched` — meaning nothing ever recorded what happened to it, because whatever was tracking it was interrupted first — and marks each `outcome_unknown`. This is what makes acceptance invariant #2 ("interrupted external effects enter reconciliation and can remain `OUTCOME_UNKNOWN`") a property this daemon actually has, not merely documents: proven by a test that creates an effect stuck `dispatched` (simulating a crash mid-dispatch), calls `ReconcileInterrupted`, and confirms it — and only it — becomes `outcome_unknown`, plus a real CLI-level smoke test (build the binary, seed a stuck effect via SQL, restart, confirm the daemon's own startup log and the database agree). `Resolve`'s terminal-outcome argument is trusted from the caller by design, not oversight — see `daemon/operations`'s doc comment for why that's the opposite posture from `daemon/selfimprove`'s server-computed verdict, and is what §4 itself requires ("AMH SHALL NOT infer that the effect failed... or select a domain recovery action").
- **Self-improvement candidate lifecycle (`daemon/selfimprove`).** `GENERATED -> EVALUATED -> CANARY -> PROMOTED | REJECTED; PROMOTED -> DEMOTED -> ROLLED_BACK`, for five candidate classes (prompt, retrieval policy, skill, module, core code). `Generate` is agent-or-operator; every other route — `RecordEval` included — is operator-only: the daemon always computes the pass/fail verdict itself from raw per-case results against a fixed threshold, but a server-computed verdict over caller-supplied measurements isn't independent if the same agent token that proposed the candidate can also submit its own passing results, so recording evidence sits at the operator tier, not the "agents propose" tier. `Promote` requires a passing eval recorded at-or-after entering canary (not merely a historical pass), demotes whatever else of the same class was previously promoted, and records that as the new candidate's rollback target so `Rollback` can restore the prior binding — failing closed if a third candidate has since been promoted over both, and serialized per class with a Postgres advisory lock (plus a partial unique index as a database-level backstop) so two candidates of the same class can never end up simultaneously promoted.

## What's declared but not yet built

Stated plainly rather than hidden in a roadmap: this repository has never obtained a live OAuth session against a real subscription provider (Codex, Grok) end to end, because the network egress this code has been developed under blocks the vendor auth domains — the OAuth refresh mechanism itself is real and tested, but has only been exercised against a fake token endpoint, not a real one. OpenAI Codex under a ChatGPT subscription is deliberately unimplemented — see `daemon/inference/inference.go`'s doc comment for why.

**No goal ever dispatches itself.** `daemon/a2a`'s `SendMessage` durably creates a real `goal` row, and `agents/workflows/goal.py`'s `pursue_goal` durably and correctly pursues one — but nothing in this codebase connects the two automatically. §3.1 lists "deterministic delivery of external triggers into DBOS using stable idempotency keys" as its own, separate amh-daemon responsibility, and it doesn't exist yet, for a goal created via A2A or by any other path: every existing test invokes `pursue_goal` directly. Until that dispatch mechanism is built, a goal created through the A2A adapter sits in `TASK_STATE_SUBMITTED` until something external runs `pursue_goal` for it.

**A2A's other scope cuts.** `daemon/a2a` implements Agent Card discovery, `message:send` (new tasks only — continuing an existing task's conversation is unimplemented), `GetTask`, `ListTasks`, `CancelTask`. Not implemented: `SendStreamingMessage`/`SubscribeToTask` (SSE streaming), push-notification config CRUD, `GetExtendedAgentCard`, multi-tenant `tenant` routing, and per-external-caller authentication (the Agent Card declares a Bearer scheme but the adapter reuses AMH's own internal agent/operator tokens — there is no separate identity system for external A2A callers yet). `CancelTask` on an already-`active` goal updates only the AMH-side record; there is no Go-side hook into DBOS's own workflow-cancellation API, so an in-flight `pursue_goal` run is not actually interrupted.

**Signing is verified but not mandatory by default.** `AMH_EXTENSIONS_REQUIRE_SIGNATURES` (`daemon/extensions.Registry.RequireSignatures`) defaults to `false` — an unsigned manifest is admitted exactly as it always was. This is a deliberate, documented scope cut, not a silent gap: every manifest shipped in this repository today (`extensions/control-plane-ui/extension.json`, the test fixtures) is unsigned, and flipping the default on would strand them without also minting and distributing real keys for them, which is a packaging/release-process decision this repository doesn't make on its own. A manifest that *does* declare a signature is always verified regardless of this setting — nothing here accepts a forged or tampered signed manifest either way. Key rotation is register-new-keyId-then-revoke-old, with no automated notification to anything that discovered an extension under a since-revoked key — revocation is forward-looking only, the same posture `daemon/credentials.RevokeAccount` already takes.

**`daemon/inference` is the only real call site so far.** `daemon/operations` is real, generic, tested lifecycle machinery, and every model-provider call now genuinely goes through it — but no MCP tool call (`agents/harness/agentic_loop.py`) and no extension activation does yet; they still complete or fail exactly as they did before this package existed, with no durable operation record and no crash recovery if the daemon dies mid-call. Inference was the unambiguous first case: it's core-owned, so `owner_extension_id` and what "reconciliation" even means had obvious answers. MCP tool calls don't have that — deciding what `effect_type`/`owner_extension_id` an MCP call should declare, and what "the owning extension reconciles" concretely means for a call that has no extension backing it (a bare MCP server, say), is genuinely separate, undecided follow-on work, not a small addition to this PR. Two more narrower, accepted gaps, both inherited from `daemon/operations` itself: this package tracks exactly one attempt per `operation_id` (`contracts/effect-record.schema.json`'s `sequence`/`attempt` fields for multi-attempt retry orchestration aren't populated — each provider in an inference failover chain gets its own fresh `operation_id` rather than sharing one across attempts), and `MarkDispatchPending`'s policy-decision-consume and its own state update are two separate transactions — a crash in the narrow window between them leaves an effect visibly stuck `admitted` with an already-consumed decision, inspectable by an operator, not silently corrupted, but not fully atomic either.

**Two of five, deliberately, not five of five.** §14 groups "backup/restore, upgrade/rollback, corruption recovery, resource exhaustion, and soak acceptance" as one line item; only the first two are built. Corruption recovery, resource-exhaustion handling, and soak acceptance testing remain undone, not silently assumed covered by having backup/restore and migration rollback. `daemon/backup` also requires `pg_dump`/`pg_restore` to be present on the daemon's `PATH` — the same PostgreSQL client tools any Postgres deployment already installs alongside the server, not an extra dependency this repository introduces, but a real deployment prerequisite worth stating rather than leaving implicit. Migration rollback's down-migration for 0003 (`ALTER TABLE goal ... DROP COLUMN status_message` / narrowing `goal_status_check` back to its pre-A2A set) will fail, as it should, if any row's status is currently `'canceled'` — that's real data with no safe automatic replacement value, not a bug.

**Self-improvement has no candidates to improve yet.** `daemon/selfimprove` is real, generic, tested lifecycle machinery — but nothing in this codebase is a candidate-GENERATING optimizer (GEPA, ACE, Voyager-style skill extraction, DSPy optimizers are named in §10 as "replaceable modules," none implemented here), so nothing calls `Generate` with a real produced candidate today. And no existing prompt/skill/retrieval-policy call site (the hardcoded prompts in `agents/workflows/goal.py`, for instance) reads from or rebinds to a `Promote`d `CandidateVersion` — promotion is real, durable state, not yet a live capability switch. "Canary" is a stricter evidence bar, not actual live traffic-splitting; no request-routing infrastructure to divert a fraction of real calls to a canary candidate exists.

## Contributing

`docs/AMH-SPECIFICATION.md` is normative — it states what the core is required to provide, what belongs to an extension, and what is explicitly excluded. Read §1 (governing decisions) and §2 (system boundary) before adding anything to `daemon/` or `agents/workflows/`; if what you're adding is domain-specific, it almost certainly belongs in an extension under `extensions/`, not in core.

`contracts/` schemas are the stable, versioned interface between core and extensions. A domain extension publishes its own namespaced schemas; it does not modify a core schema to add a domain entity.
