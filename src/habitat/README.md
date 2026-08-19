# habitat

The deterministic per-tenant runtime. Facilities, not an agent.

Owns wake, run, worker, and the admission of packs, cards, and grants.
Sequence: `event → load context → adapter thinks → validate → act → sleep`.

Planned modules: `kernel.ts` (the loop and its refusals), `stem.ts` (the
deterministic, model-free decision stem that eval can replay), `wake-log.ts`
(append-only, hash-sealed), `clock.ts` (the one kernel-owned ticker that fires
routines, deadlines, and `nextWake`), `memory-store.ts` (labeled memory, on disk).

Nothing here may promote a proposal into a live skill. Promotion is Architect + eval.
