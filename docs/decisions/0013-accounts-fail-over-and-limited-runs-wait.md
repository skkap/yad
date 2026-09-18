---
date: 2026-09-18
---

# Accounts fail over in owner order; a limited run waits

A runner can hold several **accounts** per harness, in the owner's order, each
with its own harness home. A new run takes the first account that is not at a
usage limit. A run that hits a limit mid-turn moves to the next free account at
once; only when none is free does it become **waiting** — no process, a reason
and a resume time reported in every sync, persisted so it survives a restart —
and it continues the same session with a continuation turn once a limit resets.
A hub may cap the wait, after which the run times out. While every account of a
harness is limited, the runner stops claiming for it.

This is the behaviour the operator missed most in Multica, where a limit is a
failure the hub has to notice and redo.

Sessions move between accounts because each harness's transcripts live in one
shared directory linked into every account home. **Unverified** on both CLIs; if
it fails, a session is pinned to its account and waits for that account alone.

## Considered options

**End the run as limited and let the hub resubmit** — a simple runner, and every
hub reimplementing wait-and-resume. **The hub picks the account** — puts billing
in the hub's hands, and account names into the protocol.

## Consequences

Anthropic's consumer terms may restrict using several personal subscriptions to
avoid limits. Read them before automatic failover is relied on outside Team or
Enterprise seats.
