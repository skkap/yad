---
date: 2026-09-21
---

# A hub holds a grant only while its run can still use it

[0009](0009-machine-owns-credentials-hubs-grant-per-run.md) says a grant is
destroyed when its run ends, and the runner has always kept that promise: it
strips the grants before a claim reaches `state.db`. `yad hub` did not. A run's
spec was written to `hub.db` once, grants and all, and no query ever cleared it,
so every grant the hub had been given sat in the file in plaintext for good —
and an exposed `hub.db` meant rotating every secret any run had ever carried.

**When a run reaches a terminal state — `succeeded`, `failed`, `cancelled`,
`timed_out` or `lost` — the hub blanks each grant's value and keeps its name and
delivery.** The stored spec still says what the run was given, which is what
anyone reading a finished run needs; the value, which nothing can use any more,
is gone. `lost` counts: [0023](0023-lost-stands-against-a-late-result.md) makes
it final, so a lost run is never offered again. `waiting` does not: a run parked
on a usage limit is resumed, and the resume is built from its spec.

**It is a trigger, not a query.** A run ends by a result, a cancel, a lapsed
lease, a closed session or its runner deregistering, and the next way will be
written by someone who never read this. `runs_forget_grants` in
`internal/hub/store/migrations/0001_init.sql` fires on any update that moves
`state` onto a terminal value, so no path can go round it.
`TestTerminalRunsForgetGrantValues` holds the schema to it with a bare `UPDATE`,
and `TestEveryEndForgetsTheGrantValues` walks each path the hub has today.

**Blanked must mean gone from the file.** SQLite leaves the bytes an update
frees where they were, and measured over three hundred runs a few values
survived that way in `hub.db`. Every YAD database is therefore opened with
`secure_delete`, which zeroes freed space; `TestAnEndedRunsGrantValuesLeaveTheFile`
reads the file back after a close and finds none.

## What it does not do

- **The `-wal` keeps its frames until SQLite reuses them.** Pages written while
  a run was live stay in `hub.db-wal` until a checkpoint lets the log start
  over and later writes cover them; a clean close checkpoints and removes the
  file, and a killed process leaves it (`docs/run-it-safely.md`). An exposed
  `hub.db-wal` can therefore still give up the grants of runs that ended
  recently, and `yad doctor` says so.
- **A resubmitted run is compared without values once it has ended.** A retry
  of a finished run starts nothing, so `submitRun` holds it to the grants'
  names and deliveries alone.
- **Nothing is scrubbed from a database written before this.** No hub was
  deployed when it landed, so the migration was rewritten rather than followed
  by a scrub.
- **A hub that sends a run again after `grants_lost`** sends grants its
  submitter gives it, not ones read back from `hub.db`: that run was `lost`,
  and its values are already gone.

## Considered options

**Blank the whole grants list** — simpler JSON, and a finished run no longer
records what it was given, which is the one thing about its grants still worth
knowing. **Delete finished runs** — takes the events and results with them,
which are the record a submitter reads. **Hash the values** so a retry can
still compare them — a grant need not be a random token, and a hash of a
password is an offline guess away from the password. **A store function every
terminal path calls** — correct today, and forgotten by the first path written
without it.
