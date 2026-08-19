# Autonomous Agent Habitat

A habitat for autonomous agents: a bounded team of AI agents doing real work for
one business at a time, on a real computer, under authority someone granted them
on purpose.

**Status: scaffold.** The directory structure, the toolchain, and the vocabulary
are in place. The subsystems are not implemented yet — each directory under
`src/` carries a README stating what it owns and the rules it has to enforce.

## The shape of it

- **One tenant, one computer.** A real, persistent Linux machine with a shared
  disk, covered secrets, and no network unless an allowlist is bound.
- **The team is the pack.** Agents are instantiated one-to-one from a signed
  domain pack's roles — personas, skills, specialties, and one Orchestrator.
  Nobody adds an agent by hand.
- **A deterministic kernel.** The habitat wakes the team on a closed set of wake
  kinds, runs one goal at a time, hands work to one thin worker in trailer
  isolation, and writes every wake to an append-only, hash-sealed log that
  replays with no model.
- **Authorization is the default.** Every external effect passes a deterministic
  policy gateway and then an approved card or an evidence-backed, bounded grant —
  and counts as executed only after a real send through a bound connector.
- **Two people.** The field user works journeys and decides cards. The Architect
  binds the model, connectors, skills, routines, deadlines, and mail, and can sit
  in the habitat and read its state. Shell access is not the Architect; the
  credential is.

## Layout

```
src/          the engine, one directory per subsystem (each has a README)
tests/        typed-refusal tests; no model in the loop
fixtures/     test-only packs and throwaway keys
clients/      the field surfaces (linux page, ios app)
docs/         architecture notes and decision records
migrations/   SQL for the business ledger
docker/       tenant-computer and local-stack images
scripts/      bootstrap, signing, local run
state/        per-tenant on-disk state (git-ignored)
```

## Working on it

```bash
npm install
npm run typecheck
npm test
```

Read `docs/ARCHITECTURE.md` first, then the README in whichever `src/` directory
you are about to touch. Decisions that constrain the code live in
`docs/decisions/` and are cited by id in code comments.

---

Alpha Vector LLC.
