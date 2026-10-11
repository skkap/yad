---
date: 2026-10-12
---

# A connection name given to a new registration retires the earlier one's rows

`state.db` keys sessions, runs, events, results owed and WT_SLOTs by
connection name and the id the hub chose (`0001_init.sql`). Decision 0072
made a name free again once `yad disconnect` had ended what the connection
left, so `yad connect` can give it to a different hub. The old hub's ended
rows stayed under the name. A new hub that picked an id the old one had used
had its run refused as one in a closed session, or met a kept run row with
the same id. `yad hub`'s ids are random, so it would rarely happen there. A
hub with sequential ids would meet it on its first run (DEV-166). A runner
reconnected under its default name, which is taken from the hub's host, is
the likely way in, and the refusal names nothing the owner did.

## Decision

The maintainer's (2026-10-12): **the earlier registration's rows are moved
to a retired name, `<name>~<unix ms>`.** They are kept, not deleted. The
name is not re-keyed with a generation, and reusing it is not refused. The
agent working DEV-166 decided the details below.

**How a new registration is told from the same one.** `yad connect` writes
`registered_at`, in unix milliseconds, on a connection entry it adds to
`config.toml`. Registering again with the same hub under the same entry, which
is how a lost credential is replaced, keeps the stamp, because the hub keeps
that runner's sessions. A new table, `connection_registrations` (migration
0011), records the stamp the rows under each name belong to. `config.toml` is
the CLI's to write and `state.db` the daemon's alone (0043). So it is the
daemon, at its start and before any connection's loop runs, that compares the
two for each configured connection:

- **The same stamp**: nothing moves. This is a restart.
- **No stamp in `config.toml`**: an entry made before this field. It is
  adopted with its rows, and 0 is recorded. An upgrade moves nothing, and the
  next new registration under the name retires those rows.
- **Another stamp, or none recorded**: a new registration. Every row under
  the name, in all five tables, moves to the retired name, and the new stamp
  is recorded. A name with nothing recorded and rows under it can only hold
  rows written before the table existed, by an entry that had no stamp, so
  they belong to an earlier registration.

A connection `yad connect` adds while a daemon runs is not synced until the
daemon's next start, so no loop can be writing under a name while it moves.

**One transaction.** The renames and the new stamp commit together, with the
foreign keys deferred to the commit, because every table refers to the
sessions being renamed. A crash leaves all of a name's rows moved and the new
registration recorded, or nothing changed, and the next start does it again.
A connection whose rows could not be moved is not synced at that start. Its
hub's ids would meet what the other hub left, so the connection stops with an
error naming `yad daemon restart`.

**Everything under the name moves, not only what has ended.** `yad connect`
still refuses a name whose leftovers are not ended (0072), so normally only
ended rows are left to move. If something else is there, from a hand-edited
`config.toml` or a race, it moves too. A retired name is never configured,
so the collector ends what is under it as it ends any removed connection's.
Leaving it under the reused name would send the old hub's events to the new
one, which is the hazard 0072's refusal exists for.

**A retired name never meets a configured one.** `~` is not one of the
characters a connection name may use (`config.ValidName`: `[a-z0-9_-]`).
The milliseconds move on if two retirements of a name fall in the same one.

**Workdirs and branches.** A session's first workdir is
`workdirs/<name>/<session>`, and its default git branch is
`yad/<name>/<session>`. Both are named from the connection name and the id,
so a new session that reuses an old id under the reused name derives what
the retired session used. A retired session keeps the workdir path it
recorded, and the collector reclaims it by that path. A new session in the
same directory would have its files deleted along with the old one's. The
retired session's branches stay in the bare caches after its workdir is
gone, and a new session on the same branch would start from the old hub's
commits. So a session whose id the retired registrations of its name used n
times, reclaimed or not, takes its path with `~<n>` appended, and its
default branch is `yad/<name>.<n>/<session>`. The count is taken from the
retired names, not by matching recorded paths, because a data directory
reached through a symlink or moved since would be spelled differently. A
path some row still records is skipped too. A later run of the session reads
the mark back from the path it recorded. No session id's
path name contains `~`, and no connection name contains `.`, so neither mark
can be another session's own name. Every other session keeps the names it
had.

**Retired rows are history and stay visible.** `yad sessions` lists them
under their retired name. Reclaim, the collector and the start sweeps treat
them as they treat any closed session. Pruning them by age is a separate
question that nobody has asked about yet.

**What it costs.** `config.toml` gains a key that `yad connect` writes, not
the owner. A `yad` older than this one reads `config.toml` strictly, so it
refuses the file once a connection has the key, as it refuses any setting it
does not know. Deleting the `registered_at` line makes the older binary
accept the file again. The connection is then adopted as an upgrade would
adopt it.

## Considered options

**A per-connection generation in every key.** Make the key
(connection, generation, id) and record the generation at connect. This
avoids the problem by construction. But it is a re-keying migration of every
table and every query. The loop, the reporter and the collector would also
have to carry the generation wherever they now pass the name. Renaming
retires the same rows in one transaction and changes no key.

**Delete the old rows once their workdirs are reclaimed.** Simpler, and it
leaves nothing behind. But a run's spec, its events and its result are the
only record on this machine of what a hub had it do. The maintainer chose to
keep them.

**Refuse `yad connect` under a name any row still carries.** This needs no
schema change. But every reconnect under a default name would be refused for
ever, because the old hub's rows never go. The owner would have to choose
another `--name` for a reason they cannot see.
