---
date: 2026-09-19
---

# Stop asks, then signals; restart checks before it stops

`yad daemon stop` asks the daemon over the control socket for a graceful stop.
The daemon answers first and then calls one function, `gracefulStop` in
`cmd/yad/cmd_daemon.go`. That function is the seam with drain (DEV-13). Today it
does what Ctrl-C does: the runner's context ends, and runs in flight stay held
for the next start. Drain replaces its body. The socket stays up until the
process exits, so `yad status` shows a stopping daemon and the runs it waits on.
`stop` waits `--timeout` (1m) for the lock to be released.

That is the kill fallback. It sends signals in order, and only the last is not
graceful:

- A daemon that holds its lock and does not answer on the socket gets SIGTERM
  at once. To a process that still handles signals, that is the same graceful
  stop.
- A daemon still up after `--timeout` is left alone, and the error names what
  to do next, unless `--force` was given. Then it gets SIGTERM and, 10s later,
  SIGKILL. SIGKILL skips the supervisor's cancel ladder, and harness process
  groups can outlive it, so the message says so. The runs it held are given up
  at the next start.

Draining a run can take hours, so no timeout alone may escalate to SIGKILL.
That step is the owner's choice, made with `--force`.

`yad daemon restart` checks before it stops anything: the config loads, the
socket path fits, and every connection's credential is present, `0600`,
readable and a single token. If any check fails, the running daemon keeps
running. Multica lost working daemons by restarting into a revoked login
(multica#5165). The check is local only. The one authenticated protocol call is
a sync, and a sync from a second process would renew or drop the running
daemon's leases and could be handed offers the daemon then never lists. A real
authentication probe needs a protocol call of its own, and that is in the
backlog (DEV-11).

A background start re-executes the binary as `daemon start --foreground` with
`setsid`, stdout to `/dev/null` and stderr to `logs/stderr.log`. It returns only
once that pid answers on the socket, so a start that reports success has a
running daemon, and a failed one prints its reason in the terminal.

## Considered options

**SIGKILL after the timeout by default.** It kills runs in the middle of a
drain. **`restart` as stop then start with no checks.** That is Multica's bug.
**A sync as the credential probe.** It disturbs the running daemon's leases, as
above.
