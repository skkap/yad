---
date: 2026-09-22
---

# The CLI never writes the state database, and account changes reach a running daemon live

[0035](0035-a-runner-reports-every-close-in-its-sync.md) sent `yad sessions
close` through the control socket because the state database is the daemon's.
`yad account add` and `yad account remove` did not follow it: they opened
`state.db` read-write themselves to record or clear an account's row
(DEV-26, DEV-64). A schema guard was added in front of that write, which settles
when a CLI may touch a schema it does not understand — not who owns the file.
And a running daemon held the account lists it started with, so both commands
ended by telling the owner to restart it.

Four decisions, the owner's (2026-09-22):

- **The CLI never writes `state.db`.** The daemon is its only writer. The CLI
  reads it read-only (`store.OpenProfile`) or asks the daemon over the control
  socket. A test parses `cmd/yad` and fails on any call that opens it for
  writing.
- **`yad account add` adds an account only after a verified login.** The
  label goes into `config.toml` only once the harness's own login check — the
  one the login probe uses, `account.LoggedIn` — says yes. A login walked away
  from, or a check that could not answer, leaves `config.toml` as it was, keeps
  the home for the next try, exits non-zero and names `yad account add` again.
  An account that cannot take runs is never added.
- **A running daemon takes the change up live.** Both commands write
  `config.toml` first and then send `accounts_changed` on the control socket,
  naming the account. The daemon re-reads the account lists — only those; the
  rest of `config.toml` is still read once, at start — and answers once it has
  written what follows: for an added account, its own login check's answer as
  the account's row; for a removed one, its rows forgotten. There is no restart
  notice any more.
- **With no daemon, nothing is written.** An added account has no row and a
  home on disk, which reads free. A removed account's rows are pruned by the
  daemon at its next start: whatever `config.toml` no longer lists goes.

## How the daemon holds the lists

One reloadable source, `runner.Accounts`, and every reader asks it: the
executor choosing an account, a sync's health, a parked run deciding whether it
is due, the login probe, the capability document. Before this each held its
own copy of `config.Config`, which is why nothing could change without a
restart.

A removed account cannot simply vanish, because a run may be on it with a
harness process that has the home open. So the source also knows which run is
on which account: a run takes a hold when its account is chosen and lets it go
when it moves off it, parks or ends. The hold and the removal are ordered under
one lock, so a run either holds the account before the removal — and the
removal sees it — or finds it no longer listed and chooses again. A run on a
removed account finishes there, and no new run takes it.

**Who deletes a removed account's home.** With no daemon, `yad account remove`
does. With one, the daemon does, because only it knows whether a run is on the
account: at once when none is, or when the last run on it lets go. It deletes a
home only because the owner named that account in the request, never because a
label disappeared from a `config.toml` someone edited by hand — a diff of the
lists cannot tell a removal from an edit, and the home holds a login. For the
same reason the request names the account on an add: a label already listed,
whose login a run found gone, is the owner logging it in again, and a diff
would see no change at all.

A label removed while a run was on it and then added again needs one more
step, because the login happens before the add reaches the daemon and can
take minutes: the run could end in the middle of it and delete the home being
logged in. So `yad account add` sends the same op with `keep` set before it
runs the login, and the daemon drops the pending deletion. The deletion itself
moves the home aside under the same lock and removes it after, so a `keep` or
an add that comes a moment too late finds no home and makes a new one, rather
than logging into a directory about to go. The daemon also waits for its store
before it answers a change, so the state it reports is its record and not the
default a runner with no database reads.

A daemon that did not answer leaves the home on disk and says to run the
remove again once `yad status` answers. A daemon that is gone before a held
run lets go leaves the home behind the same way; the label is already out of
`config.toml`, so nothing uses it, and the remove is safe to repeat.

## Considered options

**The CLI writes the row when no daemon runs.** The first shape of DEV-26, and
the reason the ownership rule broke: a daemon starting in the moment between
the check and the write has two writers, and the schema guard is still needed
for a CLI newer than the file. Nothing is lost by leaving it to the daemon — a
missing row with a home on disk already reads free.

**Add the label before the login, as DEV-26 did, so a half-finished login is
visible.** It made a needs-login account the ordinary result of walking away
from a browser tab, reported to every hub. Keeping the home is enough for the
retry.

**Reload the whole of `config.toml`.** Capacity, connections and permission
modes each have their own consequences for runs in flight and syncs in
progress; accounts are the part the owner changes with a command of yad's own,
and the part the ticket needed.

**Watch `config.toml` for changes.** A hand edit mid-save would be read half
written, and a removal found by a watcher is a diff — the case above where the
home must not be deleted.
