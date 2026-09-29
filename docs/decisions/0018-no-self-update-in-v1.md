---
date: 2026-09-18
status: superseded in part by 0069 — a runner updates itself when config.toml turns it on, never on a hub's say-so; the update control stays reserved and ignored
---

# No self-update in v1, but the protocol and the runner are ready for it

A customer's machine must not replace its own binary on a remote's say-so, and
there is no fleet yet to justify the machinery. v1 reserves what update needs so
nothing has to change later: a hub may declare a minimum version and refuse an
older runner with the next action ("run `yad upgrade`"), `update` is a reserved
control message, and a drain followed by re-exec is a safe restart because the
spool and outbox live in SQLite. Self-update itself is a backlog item.

## Considered options

**Hub-pushed update**, as GitHub's runners — a hub controls what code runs on
someone else's machine. **Polling GitHub releases**, as Multica — hands-off and
hub-independent; the likely shape when it is built, behind an owner opt-in.
