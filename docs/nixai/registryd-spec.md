# registryd — Identity, Headcount, and Lineage Specification

Status: draft 1. Single-host reference implementation, multi-host identity from the start.

> Read `DESIGN.md` first. This spec is a contract: the **Guarantees** are what other services and tests depend on and must hold exactly. Mechanisms described under them are the reference approach; a builder may choose differently if every guarantee and test still holds. Thresholds and defaults are in `habitat.config` and referenced by name here; never hard-code them.


## 1. Purpose

`registryd` is the authority on who exists in the habitat. It issues and retires uids, allocates subordinate uid ranges for sub-agents, enforces headcount and budget as resource physics, records lineage, and answers the NSS module so that `id`, `who`, `ls -l`, and every kernel permission check see agents and humans as ordinary users.

It stores its state as ontology objects in the `habitat` context via `ontd`. It owns no separate database. What it adds is the operations that must be atomic across the ontology and the kernel: creating a uid and its home is not a plain object write.

Nothing in `registryd` approves anything. Every precondition is a deterministic check against configured limits and the current ontology.

## 2. Guarantees

Why: agents are users. Identity, headcount, and lineage are how ownership, accounting, and isolation work without inventing anything, and headcount is resource physics rather than review.

1. Every process in the habitat runs under a uid that `registryd` issued or a subuid it allocated. No shared service accounts execute agent or human work.
2. A uid is never reused. Retired uids remain in the registry as archived objects.
3. Headcount and budget are conserved: `in_use + available = max`, and allocated budgets sum to at most the configured total.
4. Every persistent resident has exactly one `Charter`, and every type in the ontology is owned by exactly one charter.
5. Creating or retiring a resident is atomic across `ontd`, the filesystem, and the kernel. A partial creation does not exist; it is rolled back.
6. Lineage is complete: every resident records who created it and via which action; every sub-agent job records its parent.
7. A sub-agent cannot own, register, or create anything persistent. This is enforced by what its uid can reach and by the machine boundary: it runs in a disposable microVM with no dataset attached.

## 3. Objects (all in `habitat` context, stored in `ontd`)

### 3.1 Resident

Base for `Agent` and `Human`.

```
Resident {
  uid: int                       # issued by registryd, immutable
  gid: int                       # primary group, one per resident
  name: string                   # unique, POSIX-safe
  kind: agent | human
  state: creating | active | draining | retired | archived
  home: path                     # /home/<name>, snapshotting FS subvolume
  host: HostId                   # where the home is pinned
  charter: link -> Charter (one, required when active)
  budget: CognitionBudget
  created: Provenance            # creator uid, via_action
  lineage: link -> Resident (parent, optional)
  successor: link -> Resident    # set on retirement
  subuid_range: [start, count]   # agents only
  image: link -> os/Configuration (kind: machine)   # agents only; the agent's computer
  dataset: link -> os/Dataset    # agents only; persistent data attached to machines on request
  archive: path | null           # set on retirement
}
```

`Agent` adds nothing beyond `kind`. `Human` adds `session_mode: resident` (visitors are not `Human` objects; see §8).

### 3.2 Charter

As defined in the `ontd` spec: contexts, owned types, sensors that wake this resident, actions it may invoke, budget. Ownership is exclusive per type. `registryd` validates charters on creation and on every charter change through the `ontd` build.

### 3.3 Headcount

```
Headcount {
  max_agents: int                # config
  max_humans: int                # config
  budget_total: CognitionBudget  # config
  agents_in_use, agents_available: int        # derived
  humans_in_use, humans_available: int        # derived
  budget_allocated, budget_unallocated        # derived
  pending_retirements: int                    # residents in draining
}
```

Exactly one instance. Owned by the operator's charter. Config load fails if `budget_per_agent_min × max_agents > budget_total`.

### 3.4 CognitionBudget

```
CognitionBudget {
  units_per_day: int             # backend-priced units; see registryd Decision 2
  gpu_seconds_per_day: int
  source: unallocated | link -> Resident      # who this was carved from
}
```

Budgets are enforced by `cortexd` per uid as a rate limit. `registryd` only allocates and conserves them.

### 3.5 SubAgentJob

Sub-agents are not `Resident` objects. Each spawn is a job:

```
SubAgentJob {
  id, parent: link -> Agent
  subuid: int                    # from parent's range
  depth: int                     # 1..max_depth (`agents.subagent_max_depth`)
  result_schema: TypeId          # declared before spawn
  ttl: duration
  state: running | returned | expired | killed
  result: Object | null
  started, ended
}
```

The parent's budget is charged for the child's cognition. The job is the lineage record.

### 3.6 Session

One per wake for agents, one per login for humans and visitors.

```
Session {
  id, resident: link -> Resident | null      # null for visitors
  visitor_identity: string | null            # external auth subject, visitors only
  host, started, ended
  parent: link -> Session | null             # compression lineage, see cortexd spec
}
```

## 4. Actions

All actions are ontology actions with manifests, fixtures, and preconditions. `registryd` is their implementation.

### 4.1 CreateAgent

Args: `name`, `charter: Charter`, `budget: CognitionBudget`, `justification: link -> Gap`.

Preconditions, in order; all must hold, all deterministic:

1. Policy permits the caller to invoke `CreateAgent`.
2. `Headcount.agents_available > 0`.
3. `justification` links to a `Gap` object (§6): an ontology type, sensor, or workflow with no owner. The charter must claim at least one of the gap's items.
4. Charter passes the `ontd` build: no type ownership overlap, all referenced modules live.
5. Budget is available: `budget.source` is `unallocated` with enough remaining, or is the caller with enough to carve.
6. `name` is unused, including by archived residents.

Guarantees on execution (mechanism is the builder's; these must hold and are tested):

- A uid and gid are issued monotonically and never reused, and a subuid range is allocated from the global pool.
- The home dataset (from the agent template, with quota), the data dataset, and the agent's machine image (an `os/Configuration` of kind `machine` from the seed template, built) exist and are owned by the new uid before the agent is `active`.
- The uid resolves through NSS and its user session unit and cgroup slice are installed before it is `active`. Nothing is written to `/etc/passwd`.
- Charter ownership is assigned in `ontd` and the justifying `Gap` closed in the same transaction that transitions the agent to `active`.
- Headcount and budget are decremented exactly once, in the same transaction.
- If any part fails, nothing persists: no uid in NSS, no datasets, no image, no unit, no ownership. The reserved uid stays burned. A resident left in `creating` past `agents.creating_timeout` is rolled back by the `CreatingStale` sensor.
- `ResidentCreated` is emitted only after `active`.

### 4.2 RetireAgent

Args: `target: Agent`, `successor: Agent`.

Preconditions:

1. Policy permits the caller to invoke `RetireAgent`.
2. Caller has standing: `target` is in the caller's lineage subtree, or the caller holds the staffing charter.
3. `successor` is active and its charter can absorb the target's owned types without overlap. The merged charter must pass the `ontd` build.
4. `target` is not the last owner of a context with live sensors unless the successor takes the context.

Guarantees on execution:

- The target enters `draining` first and stays there until every condition below is met; `pending_retirements` reflects it.
- From `draining` on, no new observations route to the target and no new sub-agents spawn under it.
- Open yields on the target are resolved by it or deterministically reassigned to the successor after `agents.drain_timeout`; running sub-agent jobs return or expire. The slot does not free before both are true.
- Ownership of every type, sensor, workflow, and module transfers to the successor atomically in `ontd`. The `Gap` pool does not grow.
- Home and data datasets are snapshotted to the encrypted archive before the uid is disabled; the machine image is removed from the flake and its last generation's store path recorded for `RestoreAgent`.
- The uid remains resolvable (for journal and file ownership) but cannot authenticate or run.
- Budget returns to its source and the slot frees only after the archive snapshot is verified; then `retired` → `archived` and `ResidentRetired`.
- A resident in `draining` past `agents.drain_timeout` with the conditions unmet wakes the staffing charter (`DrainingStale`).

### 4.3 RestoreAgent

Args: `target: archived Agent`, `charter: Charter`, `budget`.

Same preconditions as `CreateAgent` (slot, gap, budget, build), except the name and uid are reused because they belong to the same identity. Home is restored from the archive snapshot. Lineage is preserved.

### 4.4 CreateHuman

Same as `CreateAgent` against `max_humans`, with an external auth binding instead of a session unit. Humans do not get subuid ranges.

### 4.5 SpawnSubAgent

Args: `result_schema: TypeId`, `ttl`, `context: [ObjectId]`, `task: Object`.

Preconditions: caller is an active agent or a running sub-agent with `depth < max_depth`; parent budget has headroom; `result_schema` is a live type.

Execution: allocate a subuid from the root parent's range, create a `SubAgentJob`, and have `wfd` boot a machine from the root ancestor's image with no dataset attached (OS build spec §7); the child's cognition runs under `cortexd` with the job's context and verbs. Charge the root parent's budget. On return, write `result` validated against `result_schema` and discard the machine. On TTL, `MachineStale` discards it and the job is `expired`. The subuid returns to the pool on job end.

### 4.6 UpdateCharter

Args: `target`, `charter`. Caller is the target or holds the staffing charter. Runs the `ontd` build; applies atomically or not at all.

### 4.7 MigrateResident (multi-host)

Args: `target`, `to_host`. `zfs send` of the home and data datasets, session unit re-installed on the new host, machine image already available there because the flake is shared, `host` updated. Defined now, exercised when a second host exists.

### 4.8 RetireHuman

Args: `target: Human`, `successor: Resident`. Identical to `RetireAgent`. Which groups may invoke it, and whether a co-sign is required, is a policy rule owned by whoever owns the relevant group, per `ontd` §3.8. `registryd` implements the action; it does not decide who may call it.

## 5. Headcount as physics

Headcount, budget, and slots are enforced by preconditions and conservation, not by review. Consequences:

- An agent that wants a persistent specialist must retire one it no longer needs, or the call fails.
- Budget follows headcount. Retiring an agent frees both.
- The `Gap` requirement means an agent cannot be created for a job that already has an owner, and a retirement cannot leave a job unowned.

Sensors on `Headcount`, all with fixtures and precision scores:

- `HeadcountThreshold`: `agents_available` crosses `headcount_sensors.threshold_quarter`, `threshold_tenth`, 0. Routed to the operator.
- `HeadcountRate`: more than `headcount_sensors.rate_max_per_24h` creations in 24h. Routed to the operator.
- `HeadcountTransition`: every creation or retirement. Written to the operator's log, not inbox.
- `IdleResident`: no yields resolved and no actions invoked in `agents.idle_resident_days`. Routed to the staffing charter as a standing work item: retire or justify.
- `CharterBreadth`: owned types above `agents.charter_breadth_max_types`. Routed to the staffing charter.
- `CreatingStale`: resident in `creating` past timeout. Triggers rollback.

## 6. Gaps

A `Gap` is an ontology object created automatically when a type, sensor, or workflow exists with no owning charter: at seed time, after a failed retirement transfer (which should not happen, but is detected), or when an agent defines a new type outside its own contexts. The dispatcher routes unowned observations to the triage charter, and the `Gap` object is the triage charter's work item.

Triage resolves a gap by claiming it, delegating it via `UpdateCharter` on another resident, or invoking `CreateAgent` with the gap as justification. A gap open past a threshold is itself a sensor.

## 7. Identity plumbing

### 7.1 NSS module

`libnss_habitat` answers `passwd`, `group`, and `shadow` lookups from a local read-only projection of `ontd` resident objects, refreshed on change via the bus. It never writes local files. Retired residents resolve (so journal entries and file ownership remain legible) but have no login shell and a locked password field.

### 7.2 Subordinate ranges

The global subuid pool is a `registryd` config value. Each agent gets a contiguous range on creation, sized by `agents.subuid_range_size`. Sub-agents are allocated from the root ancestor's range so that lineage is recoverable from the uid alone. A subuid holds no group memberships; a sub-agent's action invocations are evaluated by policy and the kernel as the root ancestor, with the subuid recorded as requester (`cortexd` Decision 7). Ranges are written to the NSS projection, not to `/etc/subuid`.

### 7.3 Groups

One primary group per resident. One group per bounded context, membership derived from charters. Authority groups (e.g. `staffing`, `billing-admin`, `operator`) are ordinary groups with an owner and delegates; membership is edited by the owner or a delegate via `AddMember` / `RemoveMember`, themselves policy-governed actions. Shared directories under `/srv/commons/<context>` are setgid to the context group. All groups are served through NSS so kernel permission checks and policy evaluation agree.

### 7.4 Multi-host identity

Uids and subuid ranges are globally unique across hosts from the start because `registryd` issues them centrally. On multi-host, `registryd` runs as a single writer with the same replicated change log as `ontd`, and each host runs the NSS projection locally. Client identity to `ontd` and `registryd` moves from `SO_PEERCRED` to mTLS with the uid in the certificate; the message schema does not change.

## 8. Visitors

A visitor is a human session with no `Resident` object. Authentication is external (SSO or local password), producing a `Session` with `visitor_identity` set. A visitor session runs under a per-session ephemeral uid from a reserved visitor range, with a tmpfs home, read access to what `habitat-shell` exposes, and no charter. Visitors cannot invoke actions, resolve yields, or own anything. The uid is released when the session ends. Visitor sessions are journaled under the visitor range so observation of the habitat by visitors is itself visible.

## 9. Failure behavior

- Any precondition fails: reject with the failing precondition named; no side effects.
- Failure mid-creation: reverse rollback; uid remains reserved and burned.
- Failure mid-retirement before step 5: target returns to `active`; nothing transferred.
- Failure mid-retirement at or after step 5: transfer is atomic in `ontd`; retry the remaining steps; the resident stays in `draining` and a sensor wakes the staffing charter if it stays there past timeout.
- `ontd` unavailable: `registryd` refuses all writes; NSS serves from its last projection so existing residents keep running.
- Subuid pool exhausted: `CreateAgent` fails; sensor routed to the operator.

## 10. Test obligations

- Conservation: any sequence of create, retire, restore keeps `in_use + available = max` and budget sums within total.
- Atomicity: inject failure at every step of `CreateAgent` and `RetireAgent`; verify rollback leaves no orphan uid, home, unit, or ownership.
- Uniqueness: uids and names never collide across create, retire, restore, including archived.
- Ownership: after any retirement, every type has exactly one owner and the `Gap` pool is unchanged.
- Sub-agent containment: a sub-agent cannot write outside its tmpfs, cannot reach `registryd` write endpoints, cannot exceed depth, and its subuid resolves to the correct root ancestor.
- NSS: `getpwnam`, `getpwuid`, `getgrnam` return correct results for active and retired residents; retired cannot authenticate.
- Drain: open yields and running jobs on a retiring agent are all resolved, reassigned, or expired before the slot frees.

## 11. Decisions

1. **Sub-agent max depth, subuid range size, drain timeout, creating timeout** are `habitat.config` values. Decided that they are configuration.
2. **Budget is measured in units converted from each backend's price table** (uncached input, cache write, cache read, output at their own weights), not raw tokens; `habitat.config` states totals in units. Decided.
3. **Sub-agent identity for policy: the root ancestor.** Decided.
4. **Authorization is policy, not identity kind.** `RetireHuman`, `RetireAgent`, `CreateAgent`, and every other action are governed by group membership and policy rules (`ontd` §3.8). Agents and humans are subject to identical rules. Decided.
