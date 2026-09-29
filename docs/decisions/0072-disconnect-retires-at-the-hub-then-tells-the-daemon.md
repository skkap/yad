---
date: 2026-09-30
---

# `yad disconnect` retires the runner at the hub first, then tells the daemon

A runner leaving one hub has four places to change: the hub's registration,
the connection's credential file, its `config.toml` entry, and whatever a
running daemon holds of it — a sync loop, runs in hand, open sessions and
their workdirs, parked runs, results and events owed. `yad connect` only
writes files and lets the daemon read them at its next start. The first
attempt at `yad disconnect` (DEV-29, parked on the `yad-disconnect` branch)
told the daemon in two stages either side of the hub call, and three review
rounds found five defects in the orderings that created (DEV-81).

## Decision

The owner's (2026-09-30).

**One fixed order, no stage the daemon has to remember:**

1. **Runs in progress refuse.** A run the hub has handed this runner and that
   is claimed, preparing, running or parked is named, with the command to run
   again and the one with `--now`, and nothing is retired. A running daemon is
   asked what it holds; with none, the state database is read read-only and a
   parked run counts — the next start would resume it — while a run a crashed
   process held does not, being lost already (decision 0030). `--now` goes on.
2. **The hub.** `POST /runners/{runner}/deregister` (DEV-77): the hub marks
   the runs it held lost, returns its unclaimed offers to the queue, closes the
   runner's sessions and ends the runs queued in them. Sessions are closed and
   cancelled, never unbound: a session's transcript is on the machine that
   left, so offering its continuation to another runner would be offering what
   nobody can resume.
3. **`config.toml`, then the credential file.** Under the lock every writer of
   `config.toml` takes (decision 0057).
4. **The daemon**, over the control socket (`connection_removed`), as
   `yad account add` and `remove` reach it (decision 0043). It refuses a
   connection `config.toml` still lists, so nothing can make it drop one the
   owner still has. Otherwise it stops that connection's loop and reporter,
   cancels its runs in hand, and ends what it leaves here: runs no process
   holds end `lost`, sessions close as closed by the owner — at once, or when
   the run cancelled in one has ended — and are marked reported, results and
   events owed are dropped, and the collector reclaims the workdirs. The other
   connections carry on. A daemon left with no connection stays up and
   collects, as one started with none does (decision 0035's collector, DEV-74).

**The hub records the runs lost, and `--now` stops them after it has.** Stopped
first, a run's cancelled result would race the deregister and the hub could
record it `cancelled`, or whatever it finished as; told first, the hub's `lost`
is the only end it can record, and the daemon cancels the runs a moment later
with their results going nowhere. So `--now` lifts the refusal rather than
adding a step before the hub.

The refusal is read before the hub call and is not atomic with it: a run the
daemon claims in the moment between is recorded lost by the hub and stopped
here as if `--now` had been given. Holding claims off for that moment would
need the daemon told before the hub — the two-stage shape this replaces.

**Every connection the owner removed is ended, by whichever of three ways
comes first.** The collector ends what the store holds of any connection
`config.toml` no longer lists — the daemon's copy from its start, less the
removals it has been told of. So:

- **Told live** — step 4.
- **Not told** — a daemon that did not answer. Its loop's next sync is refused
  (`unauthorized` or `runner_revoked`), and it reads `config.toml` again: a
  connection no longer listed was removed by its owner, and the refusal is the
  news, not a fault — the removal happens there and then, rather than at the
  idle TTL. A connection still listed is a fault, as before: a credential
  replaced at the hub, where the runner's sessions live on under the same id.
  Because the hub is asked before `config.toml` is edited, a sync can be
  refused in the moment between; so a refused loop reads `config.toml` again
  for up to fifteen seconds before it calls the refusal a fault, and one that
  finds the connection gone gives the owner's command two seconds to tell the
  daemon itself, so the command hears what was ended rather than that it
  already had been.
- **No daemon** — the next start's first sweep ends it. This answers DEV-74's
  question: a parked run with no `max_wait` and a claimed or running row a
  crash left belonged to a loop that will never run again, and ending them is
  what lets their sessions close.

**A name is not reused while its leftovers wait.** `state.db` keys everything
by connection name, so `yad connect` refuses a new connection a name whose
leftovers no daemon has ended yet — a new hub under it would be sent the old
one's events — and says how to end them.

**Failures stop with nothing half-removed, and the same command finishes the
job.**

- Nothing local is removed until the hub has answered. A hub that cannot be
  reached, or refuses, leaves everything as it was, and the error says so.
  `--force` goes on anyway, for a hub gone for good, and says the runner is
  still registered there.
- **Only the hub's own word makes a credential dead.** A `401` with
  `unauthorized` or any `runner_revoked` in the protocol's envelope is a hub
  that no longer knows the credential — an earlier disconnect whose local half
  did not finish — and the removal goes on without `--force`. A credential that
  cannot be read here, a missing runner id, a bare `401` from a proxy or a
  `403` never are: the hub was not asked, and a credential nobody could send
  may be a live one (finding 1 of the parked branch).
- `config.toml` goes before the credential file. A failure between them leaves
  a credential no connection names; the next `yad disconnect` of that name
  deletes it and tells the daemon. The other order would leave a connection
  whose credential is gone, which cannot ask the hub whether it is retired.
- A daemon that did not answer is an error naming the retry — which, the
  connection being gone from `config.toml`, only tells the daemon — and
  `yad daemon restart`, and saying what happens meanwhile: the refused sync, or
  with `--force` a loop that keeps syncing a hub that still takes the
  credential.

## The parked branch's five findings, in this shape

1. *Already-gone was also the never-asked state.* Three outcomes — retired,
   the hub had already retired it, still registered under `--force` — and an
   unreadable credential is refused, not taken for dead
   (`TestE2EDisconnectNeverCallsAnUnreadCredentialDead`).
2. *A refused disconnect hid the connection from `yad status`.* The daemon is
   told only after `config.toml` no longer lists the connection, so a refused
   disconnect leaves it listed and syncing
   (`TestE2EDisconnectWithTheHubUnreachable`).
3. *The daemon could exit between the stages, leaving the sessions open.*
   There are no stages; a daemon gone before it is told ends the leftovers at
   its next start (`TestE2EDisconnectWithNoDaemon`,
   `TestARunnerWithNoConnectionStillCollects`).
4. *The "no connections left" note sent the owner to `yad connect` and omitted
   the restart.* The daemon stays up collecting, and the note names
   `yad daemon restart` after the connect
   (`TestTheLastConnectionRemovedLeavesTheRunnerCollecting`).
5. *`Serve`'s comment still said it returned when every connection stopped.*
   It now says a removal is not a stop on a fault, and a runner with every
   connection removed collects until drained.

## Considered options

**The parked branch's two stages** — stop the loop before the hub, close the
sessions after. Each ordering fix made the next defect; the loop's own `401`
could arrive before the first stage, and the process could leave between the
two.

**Drop the daemon coordination: disconnect writes files and says restart.**
The ticket's live candidate, and the simplest. It leaves the sessions open
until someone restarts the daemon — or for good with `idle_ttl = "0s"` — and
the loop still meets the `401`, which then had to be tolerated anyway. The
refused-sync and next-start paths above are that design's fallback; telling
the daemon is only the fast path on top.

**Tell the daemon first, and let it deregister.** The daemon holds the
credential, so it could make the call. But the control socket answers within
seconds and the hub may take thirty; a hub that refused would leave a daemon
with a connection it had stopped and a `config.toml` still listing it — the
two-stage state again.

**End only what the removed connection's loop would have, at the next start.**
The daemon-down case alone (DEV-74). It leaves a running daemon with a loop
syncing a credential the hub has retired until its next restart.
