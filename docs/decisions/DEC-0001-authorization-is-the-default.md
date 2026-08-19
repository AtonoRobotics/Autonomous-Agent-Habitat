# DEC-0001 — Authorization is the default

**Status:** accepted
**Date:** 2026-08-19

## Context

An agent that can act on the world will, given a plausible chain of reasoning, act
on the world. The usual answers are a trust score or a tier ladder: the agent earns
a number, and the number decides. A number is not an authorization — nobody granted
it, nobody can read it back, and it drifts.

## Decision

Nothing external happens without either an approved authorization card for that one
effect, or a bounded grant that someone wrote on purpose with evidence behind it.
With neither, the system does not send and does not retry. This repository invents
no numbered trust tiers.

## Enforcement

`src/policy` runs first and fails closed with no matching allow. `src/effects`
requires the gateway *and* a card or grant, and writes `executed` only after a bound
connector returns 2xx. Card and grant payloads are tested to contain no tier number
and no "trust" score.

## Consequences

The system is slower and asks more often, especially early on, and every new action
class starts by interrupting someone. That is the intended price: the alternative is
autonomy nobody remembers granting.
