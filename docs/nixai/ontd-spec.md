# ontd — Ontology Service Specification

Status: draft 1. Single-host reference implementation, multi-host addressing from the start.

> Read `DESIGN.md` first. This spec is a contract: the **Guarantees** are what other services and tests depend on and must hold exactly. Mechanisms described under them are the reference approach; a builder may choose differently if every guarantee and test still holds. Thresholds and defaults are in `habitat.config` and referenced by name here; never hard-code them.


## 1. Purpose

`ontd` is the authoritative store of the habitat's world model and the only path through which anything reads or changes it. It holds types, objects, links, sensors, actions, steps, workflows, charters, observations, yields, and the provenance and reliability of all of them. Every other service is a client:

- `wfd` resolves workflows, steps, and actions by name and records step inputs/outputs.
- `cortexd` assembles working context from the ontology before any agent wakes.
- `registryd` stores agents, humans, headcount, and lineage as ontology objects.
- `habitat-shell` renders the object graph with live sensor state.
- Agents query, observe, and act. They never touch storage.

Ontology is not a taxonomy. A type exists only because a sensor observes it or an action changes it. `ontd` enforces that at build time.

## 2. Guarantees

Why: the ontology is the habitat's shared memory and the only world model agents perceive. Everything below exists so that ambiguity can enter only through a yield, never silently.

1. Every type has at least one sensor or one action referencing it. Types with neither fail the build.
2. Every term has one meaning inside a bounded context. Cross-context relations are translation steps, never shared types.
3. Every object, link, and module has provenance: creator uid, creating action, lineage, seed-or-agent.
4. Every module (sensor, step, action, workflow) has a manifest that fully declares its inputs, outputs, and side effects. Undeclared effects are unreachable, not just forbidden.
5. All state is derived from an append-only change log. Any index or projection can be rebuilt from the log.
6. Observations are typed objects emitted only on predicate state change. Free-text observations do not exist.
7. Reliability is computed from the journal, never assigned. Seed and agent modules carry identical scoring.
8. Nothing inside `ontd` calls a model.
9. Authority is group membership and policy, evaluated deterministically and identically for agents and humans. Module authors do not classify risk; policy owners do.

## 3. Core model

### 3.1 Context

A bounded context is a namespace in which type names are unique and unambiguous.

```
Context {
  name: string            # e.g. "billing", "fulfillment", "habitat"
  owner: uid              # resident whose charter owns the context
  description: string
}
```

The `habitat` context is seeded and holds the self-model: `Agent`, `Human`, `Session`, `Module`, `Workflow`, `Headcount`, `Observation`, `Yield`.

### 3.2 Type

```
Type {
  id: "<context>/<Name>"
  properties: [Property]
  links: [LinkSpec]
  contracts: [Contract]
  version: int
  provenance: Provenance
}
Property { name, kind: scalar|enum|ref|timestamp|blob, required: bool, enum_values? }
LinkSpec { name, target: TypeId, cardinality: one|many, required: bool, inverse?: string }
Contract { expr: Predicate, message: string }      # evaluated on every write
```

Contracts are predicates over the object's properties and links, evaluated deterministically on write. A failing contract rejects the write and emits a `ContractViolation` observation on the writer's `Module` object.

### 3.3 Object and Link

```
Object { id: uuid, type: TypeId, properties: map, version: int, provenance }
Link   { id: uuid, spec: "<TypeId>.<link>", from: ObjectId, to: ObjectId, provenance }
```

Object ids are globally unique and never reused. Type is immutable for an object; changing type is a migration.

### 3.4 Module manifest

Sensors, steps, actions, and workflows are all modules. A module is an executable plus a manifest. The manifest is the contract; the sandbox is derived from it.

```
Manifest {
  id: "<context>/<name>"
  kind: sensor | step | action | workflow | hook
  owner: uid
  description: string              # model-facing; required; states what, when to use, when not to
  inputs:  [ {name, type: TypeId | scalar} ]
  outputs: [ {name, type: TypeId | scalar} ]
  effects: [ActionId]              # actions only; empty for sensors and steps
  network: [host:port]             # declared egress; only actions may declare it
  clock: injected                  # always; modules never read the clock
  seed: int | injected             # randomness is seeded
  idempotent: bool
  preconditions:  [Predicate]      # over inputs
  postconditions: [Predicate]      # over outputs
  fixtures: path                   # required; build fails without
  version: int
  provenance
}
```

Direction rule, enforced at build:

| kind     | reads          | writes             | touches world |
|----------|----------------|--------------------|---------------|
| sensor   | world          | Observation        | read only     |
| step     | ontology       | ontology           | never         |
| action   | ontology       | ontology + world   | yes, declared |
| workflow | composes above | —                  | via actions   |

A module whose manifest implies two directions is rejected.

### 3.5 Sensor

```
Sensor extends Manifest {
  observes: TypeId
  reads: [PropertyName]
  predicate: Predicate            # over the observed object's properties
  states: [string]                # e.g. healthy | degraded | failed
  emits_on: transition            # the only value
  trigger: timer | inotify | bus_subject | poll_interval
}
```

A sensor evaluates its predicate against the observed object and holds the last state per object. It emits an `Observation` only when the state changes. Repeated evaluation with no change emits nothing. This is what keeps observations meaningful and precision measurable.

### 3.6 Observation

```
Observation {
  id, sensor: ModuleId, object: ObjectId
  from_state, to_state: string
  properties_at: map                 # snapshot of read properties
  at: timestamp                      # injected by wfd, not read by sensor
  routed_to: uid                     # filled by dispatcher
  outcome: pending | acted | dismissed | crystallized
}
```

`outcome` is written by the resident that received it. It is the raw material for sensor precision.

### 3.7 Action

```
Action extends Manifest {
  verb: string                       # e.g. "RestartService"
  targets: TypeId
  args: [ {name, type, description, enum?} ]   # per-argument descriptions are model-facing and required
  preconditions: [Predicate]         # over target and args, checked by ontd before dispatch
  implementation: StepId | executable
  result_type: TypeId                # the object the action produces or updates
}
```

Invocation path: caller → `ontd.act(verb, target, args)` → policy check (§3.8) → precondition check → dispatch to `wfd` → implementation runs in sandbox → result written as object update → sensors observe the result. The caller never touches the world directly.

The manifest says nothing about risk or reversibility. Whether an action is sensitive is decided by policy owners, not module authors.

### 3.8 Authority and policy

Standard IT security, not a new mechanism. Authority is group membership; policy maps actions to required groups; evaluation is deterministic at invocation time. Agents and humans are subject to identical rules.

```
Group {
  gid: int, name: string
  owner: uid                         # may add/remove members and delegate
  delegates: [uid]                   # may add/remove members
  members: [uid]
}

PolicyRule {
  id, owner: uid                     # must own or be delegate of every group referenced
  action: ActionId | pattern         # e.g. "billing/*"
  target: TypeId | pattern | object filter (CEL)
  requires: [GroupName]              # all listed groups
  cosign: { group: GroupName, count: int } | null   # additional distinct members must sign
  effect: allow | deny               # deny wins
  version, provenance
}
```

Evaluation at `act()`: collect rules matching the action and target; if any `deny` matches the caller, reject; otherwise the caller must be a member of every group in every matching `allow` rule's `requires`, and if `cosign` is set, a `Cosign` object from `count` other members of that group must exist for this invocation (target and args hash, unexpired, unconsumed). No matching `allow` rule means reject: default deny. Unauthorized invocations emit `Unauthorized` on the caller's object and are journaled.

Group ownership is the delegation chain. The operator owns the root groups and delegates. A group owner writes the policy for actions their group governs. Nothing in `ontd` classifies actions; it only evaluates rules.

Charters continue to define what a resident *does* (owned types, sensors that wake it). Whether it *may* invoke an action is policy. A charter listing an action the resident's groups don't permit is a build failure.

### 3.9 Yield

A yield is a typed question raised by a workflow step when determinism is insufficient.

```
Yield {
  id, workflow: ModuleId, step: string, yield_point: string
  question_type: TypeId              # the schema of the answer
  reason: string                     # why the workflow could not decide; model-facing; required
  context: [ObjectId]                # neighborhood, computed before the agent wakes
  options: [Object]                  # enumerated when possible
  routed_to: uid
  answer: Object | null
  rule_recorded: Predicate | null    # the agent's stated rule, parsed against the CEL environment at resolve time; unparseable is ResolveInvalid
}
```

`yield_point` is a stable identifier. Frequency and answer-predictability per yield point are computed properties on the `Workflow` object and are the primary crystallization sensor.

### 3.10 Charter

A charter is a query over the ontology, not prose.

```
Charter {
  owner: uid
  contexts: [ContextName]
  owns_types: [TypeId]
  wakes_on: [SensorId]
  invokes: [ActionId]                # declared use; must be permitted by policy for the owner's groups
  budget: CognitionBudget
}
```

Dispatch is: observation on type T → route to the charter that owns T. Exactly one charter owns any type; overlap fails the build. Unowned types route to the triage charter.

### 3.11 Provenance and reliability

```
Provenance { creator: uid, via_action: ActionId | "seed", parent: ObjectId | null, at }
Reliability {                         # computed, read-only, per module
  sensor.precision: acted / (acted + dismissed)
  step.replay_agreement: matching / replayed
  step.fixture_coverage: fixtures / declared paths
  module.violation_rate: violations / invocations
  module.usage: invocations
  type.instances, type.referenced_by
  computed_at
}
```

## 4. Bounded contexts and translation

Two contexts that need to share a concept do so through a translation step whose manifest declares input type in context A and output type in context B. Translation steps are ordinary steps: sandboxed, tested, replayed. There is no global type. `habitat/Agent` is the one exception every context may link to, because ownership is universal.

The build rejects a link whose `target` is in another context unless it is `habitat/Agent` or `habitat/Human`.

## 5. API

Transport: Unix domain socket with `SO_PEERCRED` on a single host; the same message schema over mTLS with the uid in the certificate on multi-host. The schema is the contract; the transport is not.

Required capabilities. Method names below are the reference names; the builder may shape the API differently provided each capability is present, typed, and scoped as stated.

**Read** (scoped to the caller's charter contexts plus `habitat`):
- fetch objects by id, singly and in batch
- exact-match query by type and property filter (no similarity)
- neighborhood: the subgraph around an object to `cortexd.neighborhood_depth`, with link filtering; this is what context assembly consumes
- full-text search over string properties, exact terms only, ranked by term frequency
- type, manifest, and reliability lookups

**Write** (validated against contracts, policy, and the caller's charter):
- create object, update with optimistic concurrency (stale version rejected, no merges), link and unlink
- record an observation; only the declared sensor's mediated write path (`wfd`) may call this
- resolve a yield with an answer and optional stated rule
- invoke an action by verb; returns an invocation id, result arrives as an object update

**Schema**:
- define type, define module (both enter the build; not live until green)
- migrate (§7)
- build: run the checks in §6 and return findings

**Subscribe**: bus subjects per §8, scoped like reads.

Every write appends to the change log before acknowledging. Every read is served from projections rebuilt from the log. Those two sentences are guarantees.

## 6. Build checks

Run on `define_type`, `define_module`, `migrate`. All deterministic. Any failure rejects; findings are returned to the caller and written as a `BuildFailed` observation on the caller's `Module` object, which wakes the author.

1. Manifest schema valid.
2. All referenced types, links, actions, and steps exist and are live.
3. Direction rule (§3.4).
4. Type has ≥1 sensor or action referencing it (invariant 1).
5. Type ownership: exactly one charter owns each type.
6. Cross-context links only to `habitat/Agent` or `habitat/Human`.
7. Fixtures present and passing in the sandbox.
8. Replay: for a changed step or action, re-run against every journaled input; every output diff is returned as a finding and raised to the author as a yield of type `ReplayDivergence`.
9. Contract chaining: in a workflow, each step's preconditions must be implied by the prior step's postconditions or by the trigger's observation type.
10. Near-duplicate check: a new type, sensor, or step whose property set or manifest is structurally similar to an existing one above a threshold is rejected with a pointer to the existing one. Merge or justify via a `DuplicateOverride` object with a stated reason.
11. Sandbox derivation succeeds: the module's closure builds and its network allowlist is unambiguous.
12. Descriptions present: `Manifest.description`, every action argument's `description`, and every workflow yield point's `reason` are non-empty and name the use and non-use conditions.

## 7. Migrations

A type change is a `MigrationPlan`: the new type version, a transform step from old objects to new, and the modules that must be rebuilt. The build:

1. Runs the transform against every existing object in the sandbox and checks contracts on the results.
2. Rebuilds every module that references the type (checks §6 on each).
3. Replays affected steps against history.

If all green, the migration applies atomically: new type version live, objects transformed, old version retained read-only for journal reconstruction. Migrations are never destructive; old objects remain reachable by `(ObjectId, version)`.

## 8. Bus contract

Subjects, NATS-style, routable across hosts from the start:

```
ont.<context>.observation.<TypeId>        # every observation
ont.<context>.object.<TypeId>.<changed>    # object create/update/link
ont.habitat.yield.<uid>                    # yields routed to a resident
ont.habitat.build.<uid>                    # build results for an author
ont.habitat.act.<ActionId>                 # action invocations, for wfd
```

Payloads are the typed objects above, serialized with a stable schema version. Consumers must be able to reconstruct state from the change log if they miss messages; the bus is notification, not the source of truth.

## 9. Storage

Storage is an interface. Reference implementation for single host:

- Change log: append-only table, one row per write, sequence-numbered. This is the unit of replication when multi-host arrives.
- Projections: objects, links, types, manifests, reliability — all rebuildable from the log.
- FTS5 index over string properties for `search_text`.
- Neighborhood queries served from an adjacency projection.
- SQLite in WAL mode is sufficient for a single host and keeps dependencies at zero.

Multi-host replaces the log with a replicated log and rebuilds projections per host. Nothing above the storage interface changes.

Snapshots of the store are taken with the same filesystem snapshot mechanism agent homes use, so a habitat can be restored to a point in time as one operation.

## 10. Failure behavior

- Contract violation on write: reject, emit `ContractViolation`, wake the writer.
- Build failure: reject, emit `BuildFailed`, wake the author.
- Stale version on update: reject with current version; caller re-reads. No merges.
- Sensor emits an observation for an object it doesn't declare: reject, emit `SensorMisbehavior`, wake the sensor's owner.
- Action invoked without policy permission: reject, emit `Unauthorized` on the caller's object. Same as a user lacking group membership.
- `ontd` unavailable: `wfd` pauses at the next step boundary; `cortexd` does not wake agents. No client proceeds on a partial world model.
- Log corruption: refuse to start; restore from snapshot. Never rebuild from projections.

## 11. Self-observation

`ontd` publishes sensors on its own objects, in the `habitat` context:

- `YieldRecurrence` on `Workflow`: same yield point resolved N times with predictable answers → wakes owner to crystallize.
- `SensorPrecisionLow` on `Module`: precision below threshold over a window.
- `TypeOrphaned` on `Type`: zero instances or zero references after grace period.
- `PrivateMemoryGrowth` on `Agent`: home size approaching cap.
- `CharterBreadth` on `Agent`: owned types above threshold.
- `HeadcountThreshold`, `HeadcountRate` on `Headcount`.
- `IdleResident` on `Agent` and `Human`: no yields resolved or actions invoked in N days.
- `SuspiciousContent` on any object: a string property containing directive-shaped text destined for model input. Left inert; routed to the object's owner.
- `MigrationRegret` on `Type`: in the window after a migration, yield rate or contract violation rate across modules referencing the type rises above its pre-migration baseline. Routed to the migration's author.

These are ordinary sensors with manifests, fixtures, and precision scores. They are how the habitat notices its own bugs.

## 12. Test obligations for ontd itself

- Property tests: any sequence of writes replayed from the log reproduces identical projections.
- Every build check has fixtures that pass and fixtures that fail.
- Contract evaluation is pure: same object, same result, no clock.
- Neighborhood queries are bounded: depth and size limits enforced, tested at limits.
- Migration round-trip: migrate forward, read old version, both consistent.
- Identity: writes from a uid without charter permission are rejected in every path.
- Bus: a consumer that misses N messages reconstructs correct state from the log.

## 13. Decisions

1. **Predicate language: CEL.** Contracts, sensor predicates, and action preconditions are CEL expressions evaluated against the typed object. No side effects, no clock, no I/O in expressions; the CEL environment exposes only the object, its links, and injected timestamps. Decided.
2. **Neighborhood depth: fixed at 2.** A spec constant, not configuration. `neighborhood()` is always depth 2 for working context. A yield that needs more than two hops does not get a deeper query; the workflow computes the additional context as an explicit step whose output is added to the yield's `context` list. This keeps the context step uniform and forces "what else does this decision need" to be declared in code. Decided.
3. Near-duplicate threshold and similarity measure for check §6.10. Needs data; start strict. Open.
4. Whether `search_text` may return approximate matches. Not yet decided. Until decided, `ontd` returns exact matches only and approximate retrieval lives in the private-memory retrieval step. Open.
