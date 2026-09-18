---
date: 2026-09-19
---

# A run starts only once the hub has acknowledged its claim

Claim by listing ([0005](0005-pull-by-periodic-sync.md), ARCHITECTURE.md §2)
leaves one window open: a run is offered in a sync response, and the hub learns
the runner has it only from the next sync. If that next sync is late — the hub
was down, the runner backed off — the offer lapses, the hub offers the run to
another runner, and the first one, if it had already started, is running the
same run twice.

So a runner records an offered run as claimed and takes its capacity at once,
but starts it only after a sync that lists it has been answered without a
`cancel` for it. To keep that from costing an interval of latency, a sync that
brings offers is followed by the next one immediately, not after the interval:
a run starts one round trip after it is offered. A run the hub cancels before
that is withdrawn — never started, capacity returned, no result owed.

On the hub's side, a listed run the runner does not hold — never offered to it,
offered to another, finished or lost — is answered with `cancel` for that run.
That is how the late runner hears it lost the race.

A run the runner will not take — a harness it cannot drive, a session it does
not hold, an invalid run — is reported as a `failed` result with error class
`refused` and the reason, so the hub stops offering it. A run past the free
capacity is simply left out of the listing: the hub offers it again later, here
or elsewhere.

## Considered options

**Start on offer**, trusting the listing to follow. One interval faster in the
worst case, and a double run whenever a sync is lost at the wrong moment — the
exact failure claim by listing exists to prevent. **An explicit claim call** per
run: the same guarantee for a second round trip every hub must implement; the
immediate re-sync gets it from the call that already exists. **Silently
dropping** a run the runner will not take: the hub re-offers it forever.
