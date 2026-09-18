---
date: 2026-09-18
---

# Runners pull work by a periodic sync

Every 10–30 seconds — the hub chooses, the runner jitters — a runner sends one
**sync** per hub: its fingerprint, the runs it holds, its health and its free
capacity. The answer carries new runs (never more than the free capacity),
control messages (cancel, interrupt, steer, drain, re-report, minimum version)
and the next interval. One call is heartbeat, lease renewal and claim at once —
Anthropic's runner model — so there is exactly one liveness signal to reason
about. Events and results are separate, pushed as they happen.

A run is taken from the local capacity pool *before* it is asked for, so a
claimed run never waits for room (Multica's lesson). The pool is shared across
hubs, offered round-robin, with the owner's optional cap per connection and per
harness.

## Considered options

**Long poll** — near-instant pickup for the same request count, but a hub holding
thousands of open requests is a real cost on serverless hosting, which is where
a Zumino is likely to live. **WebSocket wake-up hints**, as Multica — a second
channel whose failure modes (proxies, read limits, dropped hints) produced a good
share of Multica's bug history. Both remain possible later as a hub-advertised
feature; periodic sync stays the floor.
