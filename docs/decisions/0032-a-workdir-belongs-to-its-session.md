---
date: 2026-09-19
---

# A workdir belongs to its session, and a run never removes one

A resumed conversation expects the files its earlier runs left: the harness
remembers editing `main.go`, and `main.go` had better still be there. So the
workdir is the session's, not the run's (DOMAIN.md).

- **Where.** `<data>/workdirs/<connection>/<session>/`, made `0700` by the
  session's first run that reaches preparing, and recorded in the session row.
  Every later run uses the recorded path — never a recomputed one — so a change
  to how paths are named does not strand an existing session.
- **Named safely.** The session id is the hub's, so it becomes a path
  component only when it is a plain lower-case name (`[a-z0-9][a-z0-9._-]*`,
  at most 64); anything else is a hash, prefixed with `_`, which a kept name
  cannot start with. Lower case because macOS and Windows file systems fold
  case: a hub's `S1` and `s1` are two sessions, and must not share a
  directory. Run ids naming grant directories follow the same rule, for the
  same reason.
- **Kept.** A run never deletes its session's workdir, whatever its outcome —
  a failed run's files are what the next run is sent to fix. A workdir that
  disappeared between runs (deleted by hand) is made again, empty, at the
  recorded path: the conversation is in the harness's transcript, not in the
  workdir, and the harness will find out what is missing. Reclaiming is the
  hub's `close_session`, the idle TTL or disk pressure (decision
  [0011](0011-hub-closes-sessions-runner-collects.md)) — Zumino task DEV-18.
- **Last used** is the end of the session's last run, written with the run's
  result: the idle TTL runs from when the session stopped being used, and a
  three-hour run leaves it three hours younger than its start would.

What goes inside the directory — git worktrees, a `path` source, the setup
hook — is `internal/workdir`'s (DEV-16/17); the executor asks it to prepare the
directory it chose.

## Considered options

**A workdir per run**, copied or re-cloned from the last. Simple to reclaim,
and it breaks every resumed conversation that refers to a file.

**Keep any id with upper case readable, adding a hash suffix.** Readable and
collision-free, and a second naming rule to reason about; a hub that wants
readable directories can use lower-case ids.
