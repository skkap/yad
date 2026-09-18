---
date: 2026-09-19
---

# Lost stands against a late result; a run stays listed until its result lands

A runner cut off from its hub keeps working. When the partition outlasts the
lease, the hub marks the run **lost**, and the runner, back again, reports a
result for it: `succeeded`, say. The hub already holds a terminal state, and a
run reaches exactly one (DOMAIN.md), so the result is answered `409` and the hub
keeps lost. The runner drops the result from its outbox and keeps its own
record locally, for diagnosis. A result that agrees — the runner itself reports
lost — is recorded and acknowledged. Events from the run's holder still land
after it ends, lost or not, so the stream is complete for whoever reads it.

A late result is only acceptable if it is rare, so the runner keeps listing a
finished run in its syncs — as `running`, with the reason "reporting its
result" — for as long as the result is in its outbox. Every listing renews the
lease, so a hub outage longer than a lease no longer turns a finished run into
a lost one: the result arrives to a run that is still held. Lost is left for
what it means, a runner that stopped syncing.

Only the runner a run was claimed by may report on it: its events and its
result are refused with `403 not_holder` from anyone else, and nothing they
carry is applied. An offered run takes a result — the refusal of
[0019](0019-a-run-starts-once-its-claim-is-acknowledged.md) — but no events,
since a run starts only once its claim is acknowledged.

## Considered options

**The runner's result overturns lost.** The runner saw the run end, and lost is
only the hub's inference. But a hub may already have acted on lost — told a
user, queued a follow-up run — and a state that changes after it is terminal is
one no hub can build on. **Stop listing at the terminal state, as the sync
contract first said.** Simpler, and every hub outage longer than a lease would
lose every run that finished during it.
