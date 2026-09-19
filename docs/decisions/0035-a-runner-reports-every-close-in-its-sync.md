---
date: 2026-09-19
---

# A runner reports every session it closes in its sync, and never closes one in use

Decision [0011](0011-hub-closes-sessions-runner-collects.md) says a session's
workdir is reclaimed on the hub's `close_session`, after the owner's idle TTL,
and under disk pressure. DEV-18 builds it, and settles four things 0011 left
open: when a session counts as in use, how its hub hears, what disk pressure
measures, and what the owner can do by hand.

**In use is a run held.** A session with a run claimed, preparing, running or
waiting — on a usage limit or on its start time — is never closed by anything.
The close is one statement that checks this and writes the new state, so a
claim cannot slip a run in between; the claim checks the session is open in
its own transaction. A close asked for while a run is held — by the hub or the
owner — is recorded, the session takes no new run from then, and it closes
when that run ends. The first reason asked for is the one kept.

**Closing is a state; reclaiming comes after.** The session row closes first
(`closed` for a hub's or an owner's close, `expired` for the TTL and disk
pressure, with the reason beside it), and the workdir is removed after, by the
collector's own sweep: a large checkout takes a while to delete, and neither a
sync nor the owner's CLI should wait on it. A removal that fails — a git lock,
a permission — leaves the session closed and is tried again at the next sweep.
The collector removes only paths inside `<data>/workdirs/`, whatever the row
says, and never follows a symlink out of one. What preparing left outside the
workdir — git worktrees in the bare caches, `WT_SLOT`s — is undone first,
through a `Reclaim` function the git sources work supplies
(`internal/workdir`); without it, the slots alone are freed.

**The hub hears in its sync.** A runner advertising the `close_session`
feature lists every session it has closed, and why, in `closed_sessions` on
every sync until one carrying it is answered with a 2xx — at least once, so a
hub records closes by session id. The reasons are `closed` (the hub asked),
`closed_by_owner`, `expired` and `disk_pressure`: a hub can say what happened,
and each means the same for routing — a new run needs a new session. A hub
repeats `close_session` until the session appears there; a close for a
session the runner does not hold, or reported before, is answered with a
report anyway, so the hub stops asking. A run offered in a closed or closing
session is refused with the class `session_closed`. `yad hub` records a
reported close, refuses a continuation of a closed or closing session at
submit (409), and offers `POST /api/v1/sessions/{session}/close` and
`yad hub close-session`; a session no runner has claimed closes there at once,
and its unstarted runs are cancelled.

**Disk pressure is free space under the workdirs.** `statfs` on
`<data>/workdirs` (the nearest existing parent), counting what an
unprivileged process may write. Below the owner's `sessions.disk_floor`
(default 5 GiB; `"0"` turns it off), idle sessions close longest idle first —
re-measuring after each — until the floor is met or none is left. A session
idle less than an hour is left alone: a conversation's next run usually comes
within minutes, and losing its files costs more than one checkout's space. A
session with no workdir yet frees nothing and is skipped. The measurement also
fills `health.disk_free_bytes`, which the protocol had and nothing sent.

**A missing last-used stamp is unknown, never ancient.** A session whose
stamp is missing is stamped with the sweep's time, and its idle TTL counts
from there. Multica's collector read a missing stamp as the epoch and reclaimed
every workdir at once after an upgrade.

**The owner closes by hand through the daemon.** `yad sessions close <id>`
(`--connection` when two hubs used the same id) asks the daemon over the
control socket: the state database is the daemon's, and a CLI writing to it
beside a running daemon is what 0026's lock exists to prevent.

## Considered options

**Report closes as events, or in health.** Events belong to runs, and a
session closed by its TTL has none running; health is a snapshot a hub keeps
only the latest of, so a close reported once and overwritten by the next sync
would be lost. A list in the sync request, repeated until answered, is
delivered at least once with no new endpoint.

**Remove the workdir inside the close.** Simpler to reason about, and a
`close_session` in a sync answer would hold that sync — and every lease it
renews — for as long as `rm -rf` of a large checkout takes.

**Delete closed sessions' rows.** The row is what lets the runner refuse a run
in a closed session with the reason, rather than with "no such session"; rows
are small, and a session closed years ago costs a few bytes.

**Close under disk pressure regardless of recent use.** Frees space sooner,
and throws away the checkout of a conversation that is still going.
