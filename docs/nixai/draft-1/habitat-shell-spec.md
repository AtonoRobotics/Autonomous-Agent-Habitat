# habitat-shell — Human Interface Specification

Status: draft 1.

> Read `DESIGN.md` first. This spec is a contract: the **Guarantees** are what other services and tests depend on and must hold exactly. Mechanisms described under them are the reference approach; a builder may choose differently if every guarantee and test still holds. Thresholds and defaults are in `habitat.config` and referenced by name here; never hard-code them.


## 1. Purpose

`habitat-shell` is how humans operate the habitat. It is a remote client, not a desktop on the host: humans are embodied by their own bodies and machines and connect in. The shell's job is access and administration. It renders the ontology, sensor state, runs, yields, journals, groups, and policy, and lets an authorized human act on them through the same `ontd.act()` path an agent uses.

The shell has no privileges of its own. It runs as the connecting human's uid (resident) or an ephemeral visitor uid, and every read and write is subject to the same charter scoping and policy as any other account. There is no shell-only API.

Agents do not use the shell. An agent's interface is `cortexd`. The shell can show an agent's virtual display when the agent is driving a human-shaped interface, and nothing otherwise, because there is no screen in the ontology.

## 2. Guarantees

Why: humans operate the habitat; they do not have a privileged path into it. The shell renders truth and adds no capability.

1. Everything shown is an ontology object, a journal entry, or a live bus message. The shell holds no state of its own beyond session and view preferences.
2. Every write goes through `ontd.act()` as the human's uid. The shell never calls `wfd`, `registryd`, or `cortexd` directly.
3. What a human can see is what their groups and charter permit. Two humans looking at the same view may see different graphs.
4. Anything a human can do in the shell, an agent with the same groups can do through `cortexd`. The shell adds presentation, not capability.
5. Policy is readable top to bottom as a file. The shell renders and edits that file; it does not hide policy in forms.

## 3. Identity and sessions

- **Residents** authenticate against the external binding recorded on their `Human` object (SSO subject or local credential). The session runs as their uid.
- **Visitors** authenticate the same way but have no `Human` object; they get a uid from the visitor range and a session with `visitor_identity` set. Visitors see what the `visitor` group's policy allows and can act on nothing.
- Sessions are `Session` objects in `ontd`. A resident's shell session is the trigger for their inbox: yields routed to them appear here.

Transport is the same message schema as every other `ontd` client, over mTLS. The shell is a client, not a gateway.

## 4. Surfaces

Two, matching the human role.

### 4.1 Admin

For the operator and delegates. Every panel is a projection of `habitat` context objects plus the policy file.

**Headcount.** `Headcount` object live: slots in use and available for agents and humans, budget allocated and unallocated, pending retirements. Recent `HeadcountTransition` log. Threshold sensors' current state.

**Residents.** Every `Agent` and `Human`: state, charter summary, groups, budget burn today, last wake, open yields count, reliability of modules they own. Sort by budget burn, by yields, by idle days. Lineage tree view: who created whom.

**Groups and policy.** Groups with owners, delegates, members. Policy rendered as the file it is, sudoers-style, with the rules that match a selected action or resident highlighted. Editing is a text edit that submits `DefinePolicyRule` / `UpdateGroup` actions; the shell shows the build result inline. A "who can do X" query and a "what can Y do" query, both computed by evaluating the rules exactly as `ontd` does.

**Ontology.** The graph, by context. Types with instance counts, sensors with precision, actions with usage and violation rate, orphan types, near-duplicate findings. Any node opens to its manifest, provenance, fixtures, reliability history, and source tree.

**Gaps.** Open `Gap` objects with age, what is unowned, and which triage resident holds them.

**Runs.** Every `WorkflowRun` in flight: workflow, where it is in the DAG, suspended on which yield, for how long. `YieldStale` sensor state.

**Builds.** Recent build results by author: green, failed with findings, replay divergences pending.

**Backends.** `cortexd` backends with reliability, latency, and load.

**Hosts.** One host now; the panel exists so multi-host adds rows, not views.

### 4.2 Access

For anyone with permission to look at or work with a specific agent.

**Enter an agent.** Its charter, groups, budget, home size against cap, private memory fact count and trust distribution, owned modules and their reliability, open yields, running sub-agent jobs, lineage.

**Sessions.** The agent's session lineage as a tree: parent and compression children. Any session opens to its turns, tool calls, results, hook executions, and usage, straight from the journal. Read-only.

**Context.** For a live or past wake, the exact assembled working context the model saw, section by section. This is how a human answers "why did it do that": the same thing the agent saw, no more.

**Attach.** Start a conversation with the agent. This is a wake triggered by the human's session; the human's turns enter as messages attributed to their uid. The agent's tools are unchanged. Policy-governed by group.

**Display.** If the agent is currently executing an `interface/*` action (browser, GUI on a virtual display), a live view of that display. Absent otherwise.

**Source.** The module tree under `/srv/modules` for anything the agent owns, readable. Humans can read what agents built.

### 4.3 Inbox

Shared by both surfaces; the resident's own yields.

Each yield renders with its `question_type`, enumerated options as controls where they exist, and the assembled context from `AssembleContext` in the same form an agent would get. Resolving submits `ontd.resolve()` with the answer and an optional stated rule, which feeds `CaptureRule` and crystallization the same as an agent's resolution. Yields routed to a human by policy (group-required actions, co-sign requests, `ApprovalRequest`-class questions) appear here alongside domain yields.

A human resolving many yields per day is a visible number on their Residents row. It is the signal that a workflow needs a step, not that the human is busy.

## 5. Live updates

The shell subscribes to bus subjects scoped to what the session may see and updates in place. It reconciles from `ontd` on reconnect; the bus is notification, the log is truth. Sensor state changes animate on the ontology graph so the Habitat view reads as a place where things are happening rather than a table.

## 6. What the shell does not do

- No wizards that assemble objects the human cannot read as a file. Policy, charters, and workflows all have a text form and the shell shows it.
- No approval queue as a concept. Co-signs and approvals are yields in the inbox because that is what they are.
- No summarization of journals or contexts by a model. If a human wants a summary, they attach to an agent and ask; that is a wake, charged and journaled.
- No hidden admin actions. Every button is an action with a verb visible in the UI, and the same verb is available to any account policy permits.

## 7. Agent-facing display (the only agent UI)

When an agent invokes an `interface/*` action, `wfd` runs it on a headless virtual display in the action's sandbox. The display is a stream the shell can attach to for the duration of the action, and a recording is written to the journal alongside the execution record. That is the whole of the agent UI: it exists only while a human-shaped interface is being driven, and its existence is itself an `InterfaceGap` observation.

## 8. Failure behavior

- `ontd` unreachable: the shell shows last-known state clearly marked stale and disables all actions. Nothing is queued for later.
- Policy denies a view: the panel is absent, not greyed. The shell does not reveal what exists but is hidden.
- Action rejected: the rejection reason from `ontd` is shown verbatim, including the failing precondition or the policy rule that denied it.

## 9. Test obligations

- Parity: for every action reachable from a shell button, an integration test invokes the same verb through `cortexd` as an agent in the same groups and gets the same result.
- Scoping: two sessions in different groups viewing the same panel receive exactly the objects policy permits and nothing else, verified against `ontd` evaluation.
- No state: killing and restarting the shell server loses nothing but view preferences.
- Visitors: a visitor session cannot submit any action; verified for every panel.
- Context fidelity: the Context panel for a session is byte-identical to the context `cortexd` journaled for that wake.

## 10. Decisions

1. **Remote client, not a host desktop.** Web client as the reference; native clients speak the same `ontd` schema. Decided.
2. **Policy is edited as a file.** Decided.
3. **Attach is a wake.** Human conversation with an agent is journaled and budgeted like any other wake. Decided.
4. **Whether the ontology graph view is the default landing panel for Admin, or Residents is.** Proposal: graph with sensor state, since it is the habitat as a place. Open.
