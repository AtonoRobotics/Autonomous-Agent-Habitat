# Autonomous Multi-Agent Habitat (AMH)

A continuously operating habitat for autonomous agents. It wakes from goals, schedules, events, and messages; supplies durable work, context, memory, tools, workers, and recovery; and exposes stable seams on which domain applications are built.

AMH is not a governance product, a security product, a physical-device stack, or a domain application. Small hard core, reversible extensions: the core contains only the invariants every domain requires. Models, harness policies, tools, connectors, memory implementations, user surfaces, and domain behavior attach as replaceable, individually installable, individually removable extensions — see `docs/AMH-SPECIFICATION.md` §1 for the full governing decision set.

## Layout

Only directories that hold real, working code are listed — nothing here is a reserved name for future work.

```
daemon/                Go control-plane daemon (single binary)
├── cmd/amh-daemon              supervisor entrypoint (systemd/Windows Service)
├── cmd/amh-fake-device         SSH device simulator, test fixture only
├── cmd/amh-actuate             standalone CLI for one device actuation
├── supervisor/                 OTP-style supervision tree
├── scheduler/                  interval ticker for routines
├── health/                     healthz endpoint, watchdog
├── authn/                      bearer-token RBAC (agent/operator roles)
├── api/                        HTTP admin surface: actuation, approval gates,
│                                safety cases, extensions, computers, connectors, accounts
├── extensions/                 Cordis-lifecycle extension registry: discover,
│                                activate, quiesce, dispose, dependency resolution
├── sandbox/                    per-agent computer provisioning (container or
│                                Linux namespace isolation)
├── credentials/                AES-256-GCM encrypted account/module credential store
├── inference/                  model-provider seam: agents get real inference through
│                                the daemon using only their agent token, never a
│                                model-provider credential of their own
├── interlocks/                 ApprovalGate: ticket-based approval for actions
│                                with no verified inverse
├── safetycase/                 standing, revocable autonomy grants for
│                                irreversible/high-consequence actions
├── actuation/                  device-actuation kernel (physical devices — see
│                                the note on the v10 core/extension boundary below)
├── connectors/                 SSH device connector
├── observability/              OpenTelemetry tracing
└── store/                      SQLite open + migration runner

agents/                 Python cognition layer (uv-managed)
├── harness/                    tool-calling loop, scoped VFS, planning, MCP client
├── context/                    token budget manager, compaction
└── workflows/                  DBOS durable workflows: goal pursuit, actuation,
                                 approval, safety-case, and control-plane HTTP clients

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
├── manifests/                  example agent/skill/connector manifests
└── proto/                      reserved for a future gRPC daemon<->agent transport

store/migrations/       Canonical SQL DDL, applied by daemon/store and agents/workflows/ontology.py
fixtures/                test-only device simulators and a pinned MCP server
```

## What's real right now

Everything below is working code with tests that exercise it against real processes, real SQLite, and (where applicable) a real network protocol — not mocked.

- **Durable execution.** DBOS Transact on SQLite. A crashed process resumes exactly where it left off, no duplicate committed steps — proven by restart-mid-flight tests on both the goal-pursuit and greenhouse workflows.
- **Device actuation.** A real SSH connector, key-based auth, host-key verification required. Reversible actions execute autonomously with a verified inverse recorded; actions with no inverse require either an ApprovalGate ticket or an operator-approved SafetyCase.
- **Extension registry (`daemon/extensions`).** Discover, activate, quiesce, dispose against `contracts/extension-manifest.schema.json`; dependency resolution between capability providers and consumers; disposal recorded as activation's verified inverse. Knowledge base, memory, model providers, connectors, and harnesses are all just extensions to this registry — it has no domain-specific logic.
- **Computers (`daemon/sandbox`).** Each agent's own isolated compute instance: container-backed via docker, or process-backed via a real Linux mount namespace when no docker daemon is present. Create/Destroy is a verified-inverse pair.
- **Credentials (`daemon/credentials`).** AES-256-GCM encrypted at rest, fails closed with no encryption key configured, rotation-aware, never returns a secret over the admin API.
- **Control-plane UI (`extensions/control-plane-ui`).** Installs harnesses, provisions computers, configures connectors, and authenticates accounts — installed and activated through the extension registry itself, not compiled into the daemon.
- **Authorization.** Two bearer-token roles (agent, operator), constant-time comparison, fail-closed daemon startup if either token is unset. An agent token is mechanically refused on every operator-only route (extension install/activate/dispose, credential writes, SafetyCase/ApprovalGate approval) — not a convention, enforced by the server.
- **Context management.** Per-tool-result token cap, budget tracking, compaction triggered at a configurable threshold, trace-context propagation across DBOS worker-thread boundaries so a subordinate agent's span nests under its parent's.
- **MCP client.** stdio transport, tested against a real third-party MCP server (pinned, not `npx`-refetched per run).
- **Model-provider inference (`daemon/inference`).** Agents call `POST /v1/inference/{complete,count-tokens}` on the daemon using only their agent bearer token; the daemon holds the actual provider credential (registered once as a `daemon/credentials` account) and makes the real call. An `anthropic` kind speaks the native Messages API (API key or OAuth bearer); an `openai_compatible` kind speaks chat completions — between the two, Anthropic, OpenAI, Grok/SuperGrok, GLM/Z.ai's Coding Plan, and self-hosted vLLM/Ollama are all covered without per-vendor code. OAuth subscription tokens refresh and rotate back into the credential store automatically. `agents/context/llm.py` is a pure HTTP client to this seam — it holds no model-provider credential of its own. `agents/context/budget.py`'s token counting is a real tokenizer, not a `chars // 4` approximation.

## What's declared but not yet built

Stated plainly rather than hidden in a roadmap: this repository has never obtained a live OAuth session against a real subscription provider (Codex, Grok) end to end, because the network egress this code has been developed under blocks the vendor auth domains — the OAuth refresh mechanism itself is real and tested, but has only been exercised against a fake token endpoint, not a real one. OpenAI Codex under a ChatGPT subscription is deliberately unimplemented — see `daemon/inference/inference.go`'s doc comment for why.

The v10 core/extension boundary (`docs/AMH-SPECIFICATION.md` §2.3, §13, §16) states that physical devices, connectors, and safety cases for physical actuation belong in a separate Physical AI extension, not AMH core. `daemon/actuation`, `daemon/connectors`, and `daemon/safetycase` currently live in core and have not yet been moved to match that boundary.

## Contributing

`docs/AMH-SPECIFICATION.md` is normative — it states what the core is required to provide, what belongs to an extension, and what is explicitly excluded. Read §1 (governing decisions) and §2 (system boundary) before adding anything to `daemon/` or `agents/workflows/`; if what you're adding is domain-specific, it almost certainly belongs in an extension under `extensions/`, not in core.

`contracts/` schemas are the stable, versioned interface between core and extensions. A domain extension publishes its own namespaced schemas; it does not modify a core schema to add a domain entity.
