# wfd — Workflow Runner Specification

Status: draft 1.

> Read `DESIGN.md` first. This spec is a contract: the **Guarantees** are what other services and tests depend on and must hold exactly. Mechanisms described under them are the reference approach; a builder may choose differently if every guarantee and test still holds. Thresholds and defaults are in `habitat.config` and referenced by name here; never hard-code them.


## 1. Purpose

`wfd` runs the deterministic side of the habitat. It executes sensors, steps, actions, and workflows in sandboxes derived from their manifests, records every input and output to the journal, evaluates workflow triggers, raises yields when a workflow reaches a point it cannot decide, dispatches actions invoked through `ontd`, and hosts the build environment in which modules are authored and checked.

Nothing intelligent happens inside `wfd`. It never calls a backend, never spawns a sub-agent, never interprets output. A workflow that cannot proceed yields. A module that fails wakes its owner. Judgment enters only through `ontd` yields routed by charter.

`wfd` runs as its own uid. Every module it executes runs as the module owner's uid (or a subuid carved from it), so kernel permission checks apply to module execution exactly as they apply to a user running a command.

## 2. Guarantees

Why: deterministic code must be exactly reproducible and incapable of absorbing ambiguity silently. Sandboxes are closures, every execution is journaled for replay, and a workflow that cannot decide yields.

1. A module's runtime environment is derived entirely from its manifest. Undeclared inputs, network, clock, and randomness are unreachable, not merely disallowed.
2. Same inputs produce the same outputs. Any module for which this is false is a bug detected by replay, not a design choice.
3. Every step execution records its exact inputs and outputs to the journal before its outputs are visible to anything else.
4. A workflow never guesses. Any state it cannot classify deterministically is a yield.
5. Workflows contain no code. They compose modules by name.
6. No module is live until the build is green. There is no override.
7. `wfd` executes what `ontd` dispatches; it does not evaluate policy or ownership.

## 3. Modules at runtime

### 3.1 Sandbox derivation (host modules: sensors, steps, actions)

Modules are Nix derivations. The manifest's dependency list produces a closure, and the closure is the sandbox. For each execution, `wfd` builds:

- **Identity.** The owner's uid; for steps and sensors, a subuid from the owner's range so the process cannot read the owner's home. Actions run as the invoker's uid.
- **Filesystem.** A fresh mount namespace containing only the module's closure, bind-mounted read-only from `/nix/store`; declared inputs read-only at fixed paths; one writable output directory. If a path is not in the closure, it does not exist. Landlock on top as a second wall.
- **Network.** Sensors and steps: a network namespace with no interfaces. Actions: only the hosts and ports in `manifest.network`, via nftables on a veth from the namespace.
- **Clock.** Realtime clock reads blocked by seccomp; `wfd` passes the wake timestamp as an input. `CLOCK_MONOTONIC` allowed for timeouts.
- **Randomness.** Seeded from `manifest.seed` or an injected seed recorded in the journal.
- **Resources.** A cgroup under the owner's slice with limits from `habitat.config`.
- **Environment.** Empty except declared variables.

A manifest whose closure cannot be built fails the build.

### 3.1a Machine jobs (builds, tests, interface actions, agent programs)

Anything that needs a whole machine runs in a disposable microVM booted from the owner's `os/Configuration` of kind `machine`, per the OS build spec §7. `wfd`:

1. Invokes `os/Boot` with the job, whether the owner's data dataset is attached (declared in the job), declared egress, and whether a GPU is requested.
2. Delivers the job's executable, inputs, and injected timestamp/seed over vsock to the in-image job agent.
3. Streams the job's journal out over vsock into the owner's journald namespace.
4. Collects the typed result, validates it, and invokes `os/Discard`.

The `Execution` record for a machine job carries the image generation's store path as `sandbox_hash`, so replay boots the same image. A machine that exceeds the job's TTL is discarded by the `MachineStale` sensor. Nothing survives a job except what was written to the attached dataset and the journal.

Sub-agent jobs are machine jobs booted from the root ancestor's image with no dataset attached.

### 3.2 Execution record

Every execution writes, before outputs are released:

```
Execution {
  id, module: ModuleId, version, workflow: WorkflowId | null, step: string | null
  inputs: [ObjectId | scalar]           # exact values or ids at exact versions
  outputs: [ObjectId | scalar] | null
  injected: { timestamp, seed }
  started, ended, exit: ok | failed | timeout | violation
  sandbox_hash: string                  # hash of the derived sandbox spec
  owner: uid
}
```

The execution record is what replay uses. Replay re-runs the module against `inputs` and `injected` and compares `outputs`.

## 4. Sensors

A sensor's trigger determines how `wfd` schedules it:

- `timer`: a systemd timer unit owned by `wfd`, per sensor.
- `inotify`: a watch on the declared path, coalesced to at most one evaluation per `wfd.inotify_coalesce`.
- `bus_subject`: subscription; evaluation on each message.
- `poll_interval`: periodic evaluation.

On trigger, `wfd` runs the sensor in its sandbox with the observed object (at current version) as input. The sensor returns a state. `wfd` compares it to the stored last state for that object; on change, it writes an `Observation` to `ontd`. On no change, it records the execution and writes nothing. The sensor process itself never talks to `ontd`; `wfd` mediates, which is how "emits only on transition" is enforced rather than trusted.

A sensor that fails to run, times out, or returns a state not in its declared `states` is a `SensorMisbehavior` observation on the sensor's module object.

## 5. Workflows

### 5.1 Definition

```
Workflow {
  id: "<context>/<name>", owner: uid, version
  triggers: [ {sensor: SensorId, to_state: string} | {observation_type: TypeId} | {timer} ]
  steps: [
    { name, module: StepId | ActionId,
      inputs: { arg: from_trigger.field | step_name.output | literal },
      on_failure: yield | abort | retry(n) }
  ]
  yields: [
    { name, after_step: string, when: CEL over step outputs,
      question_type: TypeId, options: CEL -> [Object], context_steps: [string] }
  ]
  completion: { when: CEL, outcome_type: TypeId }
  fixtures: path
}
```

Steps form a DAG by input reference. A step runs when all its inputs are available. Independent steps run concurrently in separate sandboxes.

### 5.2 Yields

A yield point fires when its `when` predicate holds after the named step. `wfd` then:

1. Runs each of `context_steps` (ordinary steps whose outputs are the additional context this decision needs beyond the depth-2 neighborhood).
2. Evaluates `options` to enumerate answers where possible.
3. Writes a `Yield` to `ontd` with `question_type`, the enumerated options, and the context object ids.
4. Suspends the workflow run. Nothing polls, nothing spins.

When `ontd.resolve()` is called, `wfd` receives the answer on the bus, validates it against `question_type`, binds it as the output of the yield point, and continues the DAG. If the answer does not validate, the yield reopens with `ResolveInvalid` attached.

The `yield_point` identifier is stable across workflow versions unless the author renames it, so `YieldRecurrence` tracks it through edits.

### 5.3 Runs

```
WorkflowRun {
  id, workflow, version, trigger: ObservationId
  state: running | suspended(yield) | completed | aborted | failed
  executions: [ExecutionId], open_yield: YieldId | null
  started, ended, outcome: Object | null
}
```

Runs are ontology objects, so the shell can show every in-flight workflow and where it is suspended. A run suspended past `ontd.yield_stale` is a sensor (`YieldStale`) on the workflow.

### 5.4 Failure within a run

- Step fails with `on_failure: yield`: a yield of type `StepFailure` carrying the execution record is raised to the owner. This is the default.
- `abort`: run ends `aborted`, observation `RunAborted` on the workflow.
- `retry(n)`: identical re-execution up to n times; since inputs are identical, this only helps for actions with declared external effects, and the build warns if it is set on a pure step.
- Contract violation on a step's output: treated as step failure; the module's violation rate increments.

## 6. Actions

`ontd.act()` evaluates policy and preconditions, then publishes to `ont.habitat.act.<ActionId>`. `wfd` picks it up, runs the action's implementation in an action sandbox (declared network allowed), and writes the result to `ontd` as an update of the target or a new `result_type` object with the `ActionInvocationId` linked. Sensors observing the target then see the change.

Actions invoked by a workflow step and actions invoked by an agent through `cortexd` follow the identical path. There is no privileged route.

An action's implementation runs under the invoking resident's uid, not the module owner's. Kernel permissions then apply to the invoker. Policy in `ontd` says whether the invoker may call the verb; the kernel says whether the invoker may touch what the verb touches. Both must agree.

## 7. Build environment

`DefineModule` and `DefineType` from an agent or human enter the build. `wfd` hosts it:

1. A build machine is booted from the author's image (§3.1a) with the module source, its manifest, and a read-only view of the ontology schema. The build machine is the only place an agent has a shell.
2. `ontd.build()` runs the checks in its spec §6. Fixture execution and replay are delegated to `wfd`, which runs them in sandboxes identical to production.
3. Replay divergences are returned as findings and raised to the author as `ReplayDivergence` yields, one per diverging input, with old and new outputs attached.
4. On green, `wfd` realises the module as a store path (`nix build` of its derivation) and links it at `/srv/modules/<context>/<name>/<version>` for readability. A live module is a store path and is immutable by construction. Timer units and subscriptions are created for sensors.

Authoring is an action like any other, governed by policy; not every agent may define modules.

Module source is a filesystem tree, so the shell's Access view can show it and a human can read what an agent built.

## 8. The `os` context

Seeded modules wrapping what NixOS already exposes. Reads are systemd, networkd, ZFS, and cgroup state. Writes for units, timers, packages, network, and machine images go through `os/Edit` + `os/Rebuild` on an `os/Configuration`, with atomic generation switch and `os/Rollback`. Runtime verbs (`Start`, `Stop`, `Restart`, `Reload`) are the same as `systemctl`'s. Machines are `os/Boot` and `os/Discard`, always job-scoped. See `os-context.yaml`.

The test of the ontology design: if `os/Restart(Service)` is heavier to use than `systemctl restart`, or `os/Edit` + `os/Rebuild` heavier than editing a module and running `nixos-rebuild switch`, the wrapper is wrong.

## 9. Bus

`wfd` subscribes to:

- `ont.habitat.act.*` — action dispatch
- `ont.*.observation.*` — workflow triggers by observation type
- `ont.habitat.yield.resolved` — resumes suspended runs

and publishes execution and run state changes on `ont.habitat.run.<WorkflowId>`.

## 10. Failure behavior

- Sandbox derivation fails at runtime (should be impossible post-build): execution `violation`, module disabled, `SandboxFailure` on the module, owner woken.
- Module timeout: execution `timeout`, treated as step failure.
- `ontd` unavailable: runs pause at the next step boundary; sensors keep evaluating and buffer transitions locally; buffered observations are written in order when `ontd` returns.
- Bus message missed: runs and sensors reconcile from `ontd` state on reconnect; the log is the truth.
- Host restart: suspended runs are rehydrated from `ontd`; running steps are re-executed (safe by determinism; actions with external effects are re-checked against their result objects first and not re-run if a result exists).

## 11. Test obligations

- Sandbox: a step attempting to read an undeclared path, open a socket, read the realtime clock, or read `/dev/urandom` fails, for every module kind.
- Determinism: every seed module replays byte-identical over its fixture inputs across 100 runs.
- Yield suspension: a suspended run consumes no CPU; verified by cgroup accounting.
- Yield resume: an invalid answer reopens the yield; a valid answer continues the DAG with the answer bound.
- Action identity: an action invoked by uid A runs as uid A; a target A cannot write is not written.
- Sensor mediation: a sensor returning the same state twice produces one observation; returning an undeclared state produces `SensorMisbehavior`.
- Build immutability: a live module is a store path; a new version is a new derivation.
- Machine jobs: a job that writes outside its tmpfs and undeclared dataset leaves no trace after `Discard`; replay of a machine job boots the identical image generation.
- Restart: a habitat with suspended and running workflows restarts with every run in a consistent state and no duplicated external effects.

## 12. Decisions

1. **Concurrency: independent steps run concurrently; `wfd.max_parallel_per_owner` bounds it.** Decided.
2. **Default `on_failure` is `yield`.** Decided.
3. **Actions run as the invoker's uid, not the module owner's.** Decided.
4. **Timeouts, coalescing, machine TTLs** are `habitat.config` values. Decided that they are configuration.
5. **Whether workflow definitions themselves are authored in YAML or as ontology objects edited through actions.** They are ontology objects; a YAML representation exists for the shell and for reading, and is round-trippable. Proposal, open.
