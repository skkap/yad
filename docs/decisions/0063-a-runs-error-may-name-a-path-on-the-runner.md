---
date: 2026-09-29
---

# A run's error may name a path on the runner

DEV-67 (with DEV-60 before it) stopped the runner quoting what a child printed,
or an absolute path on the machine, in the documents every connected hub reads:
the capability document's errors, warnings and versions. Its sweep went one
step further, to a run's error: the executor's own failures — a harness that
would not exec, a workdir, grant directory, host-tool link or account home that
could not be made — stopped quoting their cause and sent the owner to
`yad daemon logs` instead. Other run errors, from `internal/workdir` (a link, a
lock, `git rev-parse`, a slot), kept wrapping OS errors that name a path under
the runner's data directory, and so the owner's user name. DEV-96 asked for one
rule instead of a choice made site by site.

## Decision

The owner's (2026-09-29).

**A run's error may name a path on the runner, because it goes only to the hub
that sent the run.** That hub is the one debugging the failure, and "mkdir
/Users/…/workdirs/…: not a directory" tells it and the owner more at once than a
pointer to a log only the owner can read. The same holds for a run's status
events, which go to the same hub.

The bounds, each unchanged by this:

- **What every connected hub receives names no path** — the capability
  document (a harness's or host tool's error, warnings, version) and the
  runner's health. DEV-67 stands for those: one hub must not learn the machine's
  layout from a document written for all of them.
- **A next action that is a command follows `Paths.RemoteCommand`** in any text
  that leaves the machine, a run's error included — the rule in AGENTS.md. The
  error may name the path that failed; the command it tells someone to paste
  still carries placeholders, not the owner's directories.
- **The cause the runner quotes is the operating system's, never a child's.**
  The executor names a failure's cause only when it is a `*fs.PathError`,
  `*os.LinkError`, `*os.SyscallError` or `*exec.Error` whose own error is an
  errno or exec's not-found (`osCause` in `internal/runner/executor.go`): text
  that is an operation, a path and an errno and nothing else. Every other cause
  — a database error, a sentence joined from several, an adapter's
  `LocalError` built around anything else — still ends with the command to read
  the daemon log, and the log line is written as before. That keeps a cause
  that could hold what a child printed, which can be anything including a
  token, out of the run's error by construction rather than by review. The
  child text a run's error already carried by earlier decisions is not
  widened: a setup hook's output (0034), a harness's last stderr line, git's
  last line for a failed git command.
- **A credential in a URL is never quoted back**, even to the hub that sent
  it. A git source's URL is printed through `shownURL`
  (`internal/workdir/source.go`) in every refusal, the fetch's status event
  and the run's error: `config.RedactURL` (DEV-91) takes out a URL's userinfo,
  query and fragment, an scp-like `user@host:path` loses its user the same
  way, and anything else holding an `@` is not printed at all. git's own last
  line has every URL in it redacted the same way before it becomes a run's
  error, since whether git anonymises the URL it failed to reach depends on its
  version. The hub already holds the URL; the rule is AGENTS.md's — a token is
  never transmitted — not privacy.

`internal/workdir`'s setup-hook messages keep the wording DEV-67 gave them. The
exec error for a hook names the hook, not the missing interpreter its `#!`
line points at, so the next action it carries now is the more useful sentence;
the rule allows a path there, it does not require one.

## Considered options

**No path in any text that leaves the machine**, enforced at the executor and
workdir boundary as DEV-67's failure type does for warnings. Uniform, and it
would have meant rewriting every `internal/workdir` error that wraps an OS
error, and debugging every such failure from the machine. The run's error has
exactly one reader, and it is the one that needs the detail.

**Quote the whole cause again**, as before DEV-67. The simplest change, and it
would carry into the hub whatever an error chain wraps — an adapter's error
around a child's output, a database message — which is where a token hides.
Quoting only Go's OS error types gets the path back without that.

**Refuse a git URL that carries a user over https**, since that user is
usually a token. It would keep the token out of git's argv and the cache's
config as well as out of messages. It is also a behaviour change for hubs that
send one on purpose; it is left for its own ticket.
