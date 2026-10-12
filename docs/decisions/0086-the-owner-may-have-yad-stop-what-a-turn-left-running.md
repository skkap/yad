---
date: 2026-10-12
status: accepted
---

# The owner may have yad stop what a turn left running

[0083](0083-a-run-reports-how-many-processes-its-tool-work-left-running.md)
counts the processes a turn's tool work still has running when the harness
gives its answer, and reports them as `metrics.left_running`. It does not
stop them: every harness measured (Claude Code 2.1.296, Codex 0.160.1,
OpenCode 1.18.33) starts a tool command in a process group of its own, so
yad's group kill never reaches that work, and what ends it is the harness's
own exit — if the harness stops it at all. Whether yad should stop it itself,
and whether to reach the `nohup … &` work the census cannot see, was left to
a ticket of its own. DEV-194.

## Decision

The owner decided on the ticket: **a switch in `config.toml`, off by
default.** Picked over always stopping the work, never stopping it, and
leaving the question open. Off, nothing changes from 0083. On, yad stops what
the census counted, at the moment it counts it.

```toml
[supervise]
stop_left_running = true   # absent = false
```

**The key is `stop_left_running`, under `[supervise]`.** The section is the
one that already says how yad treats a run's processes — `inactivity` and
`first_output`, the watchdogs that stop a harness — and the code is
`internal/supervise`, where the stop ladder lives. The name is the metric's:
an owner reading `left_running = 2` on a result finds the switch that acts on
those two by searching for the word. A boolean, like `[update] auto`, because
the owner asked for one switch and a stop with no setting to tune.

Not per harness (`[harness.claude]`): the census is the same for every
harness, and an owner who wants work stopped wants it stopped whichever
harness left it. Not per connection: it governs the machine's processes,
not who asked for the run, and no hub may set or widen what yad does on the
owner's machine ([0015](0015-owner-environment-is-the-trust-boundary.md)) —
nor narrow it: the switch reaches the adapter as `adapter.Spec.StopLeftRunning`,
from the runner's configuration and never from a protocol field.

**When.** Where the census is taken: after the harness's own end of the turn
and before its input closes (Claude's `settle`, Codex's `turn/completed`, the
ACP core's prompt answer). The harness is still alive there and its tool work
is still its descendants, so the census's attribution is as fresh as it will
ever be. A turn that is not measured — cancelled, interrupted, stopped by a
watchdog — is not stopped this way either; those paths already end the
harness by the ladder, and the harness its tools.

**How: the repo's stop ladder.** `supervise.Process.Stop` escalates
interrupt → SIGTERM to the group → `TermGrace` → SIGKILL to the group.
`StopLeftovers` takes the last three steps over the counted work's groups:
SIGTERM to each, up to `DefaultLadder.TermGrace` (5 s) for them to go,
rereading the process table every 100 ms, then SIGKILL to the groups that
still hold counted work, and a second to see them gone. There is no interrupt
to send: the work is not the harness, and has no protocol.

**Which groups — the safety rule.** A group is signalled only when a process
table read just before that signal shows it holding a live process the
census counted: the same pid, with a start time (read off `ps`'s elapsed
time) within two seconds of the census's. A group with a live member cannot
have been given away, so its id still names the work's group and not one an
unrelated process made after the work's emptied. A pid reused within two
seconds of the process it replaces would need the pid space to wrap in that
time. And never:

- **pgid 0 or 1**;
- **the daemon's own group** (`getpgrp`) — a guard against a table that says
  something impossible, since a harness is started in a group of its own;
- **the harness's group** — its helpers, Claude's MCP servers among them
  (0083's M3), live there, and the harness itself;
- **a group holding a live named helper** — Codex's `codex-code-mode-host`
  sits in a group of its own beside the work, and a counted descendant left
  in its group would take it down with it.

What is left is the moment between the table and `kill(2)`, in which every
member of the group would have to exit and the id go to a new group leader —
pid wraparound within milliseconds. No portable call closes it: Linux's
`pidfd_send_signal` addresses a process, not a group, and macOS has nothing
of the kind. A table that cannot be read signals nothing, rather than act on
the last one.

A zombie is not running: work the ladder ended under a harness that has not
reaped it yet is stopped, and is not killed again.

**What the result says.** `metrics.left_running` keeps its meaning — what the
turn left, whoever stopped it after — so it reads the same with the switch on
or off and a hub's warning on a hollow success still fires. The `left_running`
status event ends, with the switch on, by saying what yad did: `…: sleep
(running 4s); yad stopped it`, `yad stopped them`, `yad could not stop
them`, `yad stopped 2 of them and could not stop 1`. No protocol field: the
event's text is free and already the place a hub shows the names, and a
`left_running_stopped` count would be a new public field for something only
the owner's configuration decides. The runner's log marks each pid `stopped`
or `not stopped`.

**The `nohup … &` gap stays.** A process whose shell backgrounded it and then
exited is init's at once (0083's M2, M5b), in a group whose leader is gone,
and nothing on macOS ties it back to the run. It is neither counted nor
stopped, and it outlives the turn. Linux's `PR_SET_CHILD_SUBREAPER` — on the
daemon, or on a reaper per run — would keep it reachable; the owner decided
against adding one now. It would make Linux and macOS count differently for
the same run, and a daemon that is every run's subreaper reaps every
orphan of every run, which is a second job the supervisor does not have.

## Considered

- **Always stop** (the ticket's first option). Work a run means to leave —
  a dev server for the next run in the session, a watcher — would die with
  every turn, on every runner, from the day the release landed. The owner
  chose the switch.
- **Never stop, count only** (0083 as it stood). The harnesses measured stop
  their own tool work as they exit, so today this mostly works; it is the
  owner who cannot rely on a harness release keeping that true.
- **Stop after the turn, in the runner, once the adapter returns.** By then
  the harness has exited or been stopped — Claude waits for yad's SIGTERM, 10
  s after its input closes — and its children are init's; the census's pids
  and start times would still identify them, but the harness has usually
  stopped them first, and "yad stopped it" would mostly mean "it was gone".
  Stopping where the census is taken is the moment the ticket names.
- **Signal each counted pid rather than its group.** The same reuse question
  per pid, and it misses what a counted process starts in its group between
  the census and the signal — the shell's `sleep` started a moment later.
- **Signal the census's groups without rereading the table.** The id of a
  group that emptied between the census and the signal could by then be
  another process's group. One more `ps` per signal is the price of never
  sending a signal on a guess.
- **A protocol field saying how many were stopped.** Additive and allowed,
  but a public field forever for what one owner's switch decides. The event's
  text carries it.

## Consequences

- Off by default, so nothing changes for a runner whose owner does nothing.
- On, a turn that left work running ends up to 5 s later while the work
  handles SIGTERM, plus a second when one needs SIGKILL; one `ps` every
  100 ms meanwhile, and only when the census found something.
- On, work the harness would have kept past the turn — Codex's background
  terminal, Claude's `run_in_background` — is stopped at the turn's end
  rather than at the harness's exit a moment later. A run that needs a
  process to outlive its turn needs the owner to leave this off.
- The harness sees its tool processes die while its input is still open.
  Its answer is already in hand, and the adapter is closing the turn; what a
  real harness does on seeing it — a notice in its stream, nothing at all —
  is not measured. The tests drive fakes that play the shapes 0083
  measured.
- What the census does not count, yad does not stop: `nohup … &` work, and
  tool commands a future harness runs inside its own group.
