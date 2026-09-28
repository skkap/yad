---
date: 2026-09-29
---

# A hub holding work for a runner asks it back in 3 s

A run queued behind another starts up to one sync interval — 15 s from
`yad hub` — after the run ahead of it ends, because the runner hears about it
only at its next sync ([0005](0005-pull-by-periodic-sync.md)). In an
interactive session that is every turn: the next one is submitted while the
last runs, and waits for the session's one live run to end. The hub owns the
timings, so it can close most of that gap without a new capability.

**`next_sync_ms` may go down to 3 s**, where the steady interval, and
register's `sync_interval_ms`, stay bounded 5–60 s. A runner's floor for
`next_sync_ms` is 3 s to match; a runner from before this clamps to 5 s and is
only slower. Conformance checks each field against its own floor. The wire
does not change, and no field is renamed or removed.

**`yad hub` answers 3 s** — the owner's number (DEV-53, 2026-09-29) — while a
queued run waits for that runner and nothing but a run it is executing: its
capacity is full, or the run is the next in a session it holds. Precisely,
a queued run passes every rule an offer to it applies except capacity and its
own session's live run, and a run the runner lists as executing — not
`waiting` — would let it go by ending: the session's live run, or, for
capacity, a run of its harness when that harness's own cap is full and any run
otherwise. Otherwise the configured interval, unchanged.

The two conditions are what keep a runner from being asked back for nothing:

- **Something it is executing must free the run by ending.** A waiting run
  gives its capacity, and its session, back at an account's reset, hours off. A runner listing nothing has its
  capacity taken by what this hub cannot see end — another hub's runs, or a
  pool of none. Either asked back every 3 s would sync a thousand times and
  find itself no freer. The cost is that a runner full of another hub's runs
  picks this hub's up an interval late, as before. And a run ending frees
  only its own harness's cap, so a claude run going on does not count for a
  codex run held back by codex's.
- **The queued run must be one it would take.** A harness it cannot drive or
  has capped at zero, a session bound to another runner or closing, a start
  moment it cannot hold, an effort it does not take, a source on the machine
  its owner has switched off: none shortens anything, so an idle fleet and a
  run no runner here can take keep the normal interval.

The price is a runner at capacity for the length of a long run syncing five
times as often for that long. Against a single-writer SQLite hub serving a
handful of runners that is nothing, and it is paid only while work waits.

## Considered options

**3 s for any queued run at all** — simpler, and it makes every runner of a
fleet spin at 3 s for as long as one run nobody can take sits in the queue.

**Only what could be offered now** — the literal reading, where capacity is
all that holds a run back. It leaves out the session's next turn, which is the
case the ticket was written for.

**Holding a sync open until work arrives** closes the rest of the gap, the run
submitted to an idle runner a moment after its sync, which no answer already
sent can change. It is a new protocol capability, not a timing, and is DEV-49.
