# DESIGN.md — The Habitat

Read this before any spec. The specs are contracts between services. This is why they say what they say. When a spec is silent, decide the way this document would.

## What this is

An operating system whose primary resident is an autonomous agent. Linux kernel, NixOS, and a user space organized around agents the way a conventional OS is organized around a human at a keyboard. Humans are residents too, and administrators, but the habitat is the agent's body.

It is not a governance product. It is not an approval workflow with a model attached. It is not a demo of agents doing theater. It is an OS.

## The thesis

**Agents provide cognition. Deterministic code provides repeatable processes.** Everything follows from taking that seriously.

A workflow never guesses. When it reaches a state it cannot classify deterministically, it yields: it stops, wakes the agent that owns it with a typed question and the context needed to answer, and waits. An agent never repeats. When it is woken for the same class of decision more than once, its obligation is to turn that decision into code so it is never asked again. We call that crystallization, and it is the product. Over time, yields per workflow trend to zero, agents wake less, and their job shifts from deciding to authoring.

Intelligence is not to be wasted, in carbon or in silicon. Every place the design spends a model call has to justify it against the alternative of code.

## Why an ontology

An agent reasoning over raw data has an open hypothesis space. An agent reasoning over an ontology has a closed one: these types, these relations, these verbs. Ambiguity doesn't vanish, it becomes enumerable, and an enumerable question is a decision rather than a research project.

The ontology is the habitat's shared memory and its world model. Sensors are deterministic processes bound to ontology objects with a health predicate; they emit typed observations only on state change. Actions are typed verbs with deterministic implementations; agents choose the verb and arguments, the verb does the deed. The agent never touches the world except through actions and never perceives it except through sensors. Observe, reason, act, where observe and act are code and only reason is cognition. That is what stops AI from becoming all things.

Rules for keeping an ontology honest, all enforced by the build rather than by discipline:

- A type exists only because a sensor observes it or an action changes it. Otherwise it is documentation, and documentation is not ontology.
- Every term means one thing inside a bounded context. Cross-context relations are translation steps, never shared types.
- The ontology grows from yields, not from upfront modeling. A recurring yield is evidence of a missing type, sensor, or action. Model what is decided; let the rest arrive.
- The failure mode is not too little structure. It is near-synonymous types accumulating until the ontology reintroduces the ambiguity it was meant to remove.

Palantir's ontology work is the closest prior art: model the decisions, not the data; put business logic in published functions the model calls rather than rules it guesses; keep automation separate from cognition. We deliberately do not import their default of proposals awaiting human approval.

## Why agents are users

An agent is a uid. It has a home, groups, a charter, a journal, and a budget. Every process in the habitat belongs to exactly one uid. This is not a metaphor; it is how identity, ownership, isolation, and accounting work without inventing anything.

Nothing in the habitat checks what kind of principal is calling. Services, agents, and humans are uids in groups, and the only questions are "is this account in the group" and "what does the policy say." Authority is group membership. Policy maps actions to required groups and is evaluated deterministically, default deny, the way sudoers and polkit evaluate a request. Group owners delegate. That is the whole authority model, and it is not new because IT security already solved it. If agents ever prove they cannot be contained by it, we revisit; until then, we do not reinvent it.

Consequences worth stating: the manifest author of an action does not decide whether it is dangerous; the policy owner does. Whether a co-sign is required is a policy rule, not a mechanism baked into any action. An agent administering its own machine image needs no permission because it owns the image; an agent editing the host configuration needs `os-admin` because it doesn't.

## Why headcount

Persistent agents cost money, like employees. There is a configured number of them and no exceptions path. Creating one requires a free slot, a budget, and a gap: an ontology type, sensor, or workflow that nobody owns. Retiring one requires a successor who absorbs its ownership, so retirement never creates a gap. An agent that wants a specialist retires one it no longer needs, or the call fails.

Sub-agents are free. They are ephemeral, own nothing, spawn for one job in a disposable machine, return a typed result, and are gone. Fan-out is not reproduction. The only reason for a persistent agent is continuity: memory across jobs, ownership of a domain, the obligation to crystallize. A well-run habitat should trend toward fewer persistent agents over time as their work becomes workflows. Headcount growing monotonically means crystallization is broken, and that is a sensor.

## Why the agent's interface is three calls

Read the ontology. Receive typed observations and yields. Invoke typed actions. That is the entire surface cognition can reach. No shell, no filesystem, no network from the model's point of view; those are actions in the `os` and `interface` contexts, exposed only to agents whose charter and groups include them.

This is not restriction for safety's sake. It is what makes the agent's world unambiguous and its context assemblable by code. The ontology is an index over the OS, not a replacement for it. An agent in `os-admin` edits systemd units and network config through `os/Edit` and `os/Rebuild` exactly as an admin would, with a typed result instead of stdout to parse. If a wrapper is heavier than the command it wraps, the wrapper is wrong.

Human-shaped interfaces (browsers, GUIs, shells) exist because the world made them that way. Using one is journaled as an `InterfaceGap`, because the second time an agent drives the same form, the right answer is a sensor and an action.

## Why context is assembled before the agent wakes

An agent that has to go find its own context wastes the first half of its budget. So the habitat assembles working context deterministically before the model is called: the trigger, the depth-2 ontology neighborhood, reliability annotations on everything in it, private memory, the rolling summary, open items. Synchronous; no wake proceeds on partial context. A yield that needs more than two hops declares extra context steps in the workflow. Context assembly is the single biggest lever on not wasting intelligence.

## Why memory is mostly not in the agent

Four tiers. Working context, assembled per wake. Episodic memory, the append-only journal, which doubles as the replay source for the build. Private memory, the agent's home: a typed fact store with a hard cap and computed trust. Shared memory, the ontology.

Private memory is uncrystallized knowledge and should be small. A rule kept in a note instead of a step is intelligence stored where it cannot be reused, tested, or inherited. The cap exists to force knowledge into the ontology. Sub-agents have no private memory at all.

The Hermes finding that shaped this: a long-running agent survives context compression only if its memory provider is always available at prefetch, returns exact facts rather than similar ones, has no model in the write path, scores trust and decays stale facts, and preserves relations. Holographic memory was that provider; it is a local, untyped, trust-scored fact store with relational queries, which is an ontology with the schema left off. Our answer is to never be in a position where anything needs re-hydrating from prose: compression rebuilds context from the ontology and journal, with the summary as one input among several, and the summary itself is typed facts.

## Why no thinking twice

A hook is deterministic code that runs inside a wake at a lifecycle point. Where it needs judgment about something *other than the decision the agent just made* (summarizing a session, extracting facts from a long result), it spawns a sub-agent with a result schema. A hook never re-evaluates the agent's own decision. An agent that thinks twice about one action is a wake whose context was incomplete, and the fix is the context, not a checker. Multiple calls are fine when they are about different things.

All cognition goes through the front door: every model call is a wake or a sub-agent job, attributed, budgeted, journaled, and limited to the three-call interface. What that protects against is hidden cognition you cannot see, bill, or replay.

Nothing about context pressure, budget pressure, or compression is ever visible to the model. Hermes removed intermediate pressure warnings because they made models give up prematurely; we never had them.

## Why NixOS and disposable machines

The thesis wants deterministic code to be exactly reproducible and every runtime derivable from a declaration. NixOS is the OS built on that thesis. A module's manifest produces a closure, and the closure is the sandbox: if a path is not in it, it does not exist. Replay against last year's journal runs on last year's libraries. Configuration changes are atomic generation switches with rollback. A live module is a store path, immutable by construction.

An agent's identity stays on the host. Its computer is a microVM booted per job from its own image and discarded after. A build or test always runs on a clean, known machine, so green means something. A browser session cannot carry state into the next job. Replay is boot-the-same-image-run-the-same-inputs. An agent cannot accumulate undeclared state in its computer, only in its dataset. That is the point of having a VM.

Humans operate from workstations: NixOS with Hyprland, keyboard-first, and the habitat shell as the desktop's primary surface rather than a bar and a launcher. Fast because Hyprland is fast and nothing else runs.

## Why the shell adds no capability

Everything a human can do in the shell, an agent in the same groups can do through `cortexd`, and a test enforces it. The shell renders the ontology, the journal, and the bus. It edits policy as a file. It has no approval queue, because co-signs are yields in an inbox. It never summarizes anything with a model. It shows, for any wake, the exact context the model saw, byte-identical to the journal. That is the honest answer to "why did it do that."

## Correct beats done

Done wrong is not done. There is no minimal version, no day one, no phase that ships less than the design. The build is the gate and the build is code: types check, fixtures pass, history replays, contracts chain, near-duplicates are rejected. Red wakes the author. Green activates. There is no override.

Every unhandled case is either a typed observation or a build failure. Nothing in between.

## Habits for the builder

- When the spec is silent, do what this document would do.
- When a mechanism in a spec seems arbitrary, check whether it is a guarantee or a suggestion; guarantees are marked, and mechanisms behind them are yours to choose if the guarantee and its tests hold.
- Thresholds and defaults live in `habitat.config`. They are configuration, not design. Do not hard-code them.
- Do not add a check, a gate, or a confirmation that the design does not have. If something feels unsafe, the answer is a policy rule the operator writes or a sensor that wakes an owner, never a hidden step.
- Do not use a model where code would do. Do not use code where the answer requires judgment; yield instead.
- Name things with the OS's names. `ActiveState`, not `status`. If the OS already has the concept, transcribe it.
