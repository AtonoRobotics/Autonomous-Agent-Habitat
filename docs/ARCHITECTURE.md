# Architecture

This is the map. Each subsystem's own README states what it owns; this file states
how they fit and which rules cross boundaries.

## One loop

```
wake → load context → adapter thinks → validate → act → sleep
```

The habitat kernel owns that loop and is the only thing that writes a run. It is
facilities, not an agent: it does not think, and it holds no opinion the pack did
not give it.

## The pieces

| Piece | Owns | Refuses |
|---|---|---|
| `src/packs` | the signed domain pack: roles, journey kinds, action classes, policy bodies, ceilings, fixtures | unsigned, half-signed, incomplete, or tampered packs — at load and on every read-back |
| `src/habitat` | wake, run, worker, wake-log, clock, labeled memory | a second open goal per tenant; heavy work on a talking pass; self-promotion of a proposal |
| `src/agents` | agent records from `roles[]`, personas, inter-agent mail | agents added by hand; authority claimed through mail |
| `src/policy` | the deterministic gateway in front of every effect | any matching deny; no matching allow (fails closed) |
| `src/auth` | principals, field tokens, authorization cards | a field token on an Architect verb; silent resubmission after a deny |
| `src/grants` | bounded grants per action class | graduation without independent evidence, eval ids, and a plain-language notice |
| `src/effects` | the executor: card or grant → connector → `executed` | writing `executed` without a real send |
| `src/connectors` | Architect-bound world handles and their credentials | sending on an unbound connector or leaking a secret to a model |
| `src/computer` | the tenant's Linux machine, its disk, its egress | network without a binding; paths that escape after resolution |
| `src/eval` | pack fixtures and model-free replay of the wake log | self-declared success as evidence |
| `src/surfaces` | the field path and the Architect verbs, including the read-only seat | configuration on the field surface |
| `src/data` | PostgreSQL as the only business truth | booting without a database |

## Rules that cross every boundary

1. **Authorization is the default.** No grant and no card means the system does not
   send and does not retry. There are no numbered trust tiers.
2. **Deny is terminal.** The same agent, action class, subject, and channel cannot
   come back around under a new phrasing.
3. **Graduation never strips the gateway.** An authorized grant changes who has to
   be asked, never whether policy runs.
4. **Evidence is immutable.** It is written by the core or the acting agent and
   never edited afterwards.
5. **Memory is labeled and is not fact.** It is injected into the next think pass —
   memory that is not injected is a diary — and it never becomes fact, consent, or
   outcome.
6. **Every refusal is typed.** A refusal returns a code the field language map can
   render and a test can assert. Silence is not a refusal.
7. **The seat only reads.** Sitting in the habitat wakes nothing and writes nothing.

## What is deliberately absent

No chatbot, no copilot, no architecture console on the field surface. No trust
ladder. No hosted vendor cloud in source: the Architect writes the model URL, the
connector URL, and the credentials. No named Architect desktop — the seat is a
record you read.

## Order of work

1. Types, ids, and the wake-log — the spine everything else is asserted against.
2. Pack loading and signature verification, with failing-closed tests first.
3. The kernel loop against a fixture adapter, with no model in CI.
4. Policy gateway, cards, grants, then the executor and connectors.
5. The tenant computer and trailer isolation.
6. The field surface, then the Architect verbs and the seat.
