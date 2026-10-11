---
date: 2026-10-12
---

# The owner declares an environment for everything yad starts

A runner installed as a service keeps the PATH the owner's login shell had
([0028](0028-a-runner-is-a-per-user-service-with-its-login-path.md)) and no
other variable of it. On Ashikaga Docker runs on colima, so a run had no
`DOCKER_HOST`, and Testcontainers failed until a worker found the socket
override itself (JapanLaw #1, finding 7). The owner also had no way to give
every run a harness setting such as Claude Code's `BASH_MAX_TIMEOUT_MS`, and
a test suite longer than Claude's 10-minute command cap was backgrounded
instead — the hollow successes DEV-186 is about. DEV-188. Amends
[0060](0060-the-owners-own-account-variables-are-removed-from-every-run.md),
whose scrub this layer sits on.

## Decision

The owner's (2026-10-12): **a top-level `[env]` table in config.toml, given
to every child yad starts** — harness turns, setup and teardown hooks, the
docker probe, login checks — not the ticket's `[run] env`, which would have
reached runs alone. The rest was settled while building it.

- **A layer inside `supervise.Start`**, between the scrubbed environment and
  the spec's own `Env`: `supervise.Environ` is the process's environment
  scrubbed, then `[env]`. It is set once per process when config.toml has
  loaded and before the first child — the daemon before its first
  capability build, `yad doctor`, `yad harnesses`, `yad connect`, and
  `yad account add`, whose interactive login is the one child not started by
  `Start` and takes `Environ` too. So a check runs in the environment its run
  will have, as [0045](0045-runs-use-the-host-tools-detection-resolved.md)
  and 0060 require. What yad sets for one child still wins: an account's
  home and token, a hook's `WT_*`, the host-tool `PATH`, a git command's own
  variables.
- **Refused at load, from one rule in `supervise`** (`OwnerRefusal`, in an
  untagged file so `internal/config` can call it): every name
  `v1.AccountVariable` lists, the harness homes included; everything `Scrub`
  removes (`CLAUDECODE`, `CLAUDE_CODE_*`, `YAD_*`); `IS_SANDBOX`, which yad's
  root check and doctor read from the runner's own environment and `[env]`
  would not change; `PATH`, `HOME` and `WT_*`. Each refusal names the rule and
  the way round it, and a test reads the grant list's source so a name added
  there is refused here too. A refused name stops the daemon starting — a
  runner quietly running without a variable the owner declared would fail as
  it did before, with nothing saying why — and doctor prints the reason.
- **Strings only, taken as written.** No `$VAR` or `~` is expanded; a number
  or a table is refused with the fix ("quote it"), a NUL byte because no
  environment can carry one. Read at the daemon's start, like the rest of
  config.toml but its account lists.
- **A hub's grant under a declared name refuses the run**, at the claim and
  again where grants become variables, for a run claimed by a process whose
  config declared less. Never dropped ([0024](0024-grants-are-named-as-secrets.md),
  [0038](0038-the-owner-trusts-the-hubs-it-connects.md)) and never let win
  ([0040](0040-a-grant-may-not-move-a-run-off-its-account.md)): the message
  names the grant the hub sent and nothing else of `[env]`. Which names are
  declared is not published to hubs.
- **Values never leave the machine through yad.** `yad config apply` reports
  an `[env]` change as set, changed or removed; doctor and the daemon's log
  name variables, never values; `adapter.Mask` replaces `[env]` values in the
  stderr a run's error quotes, as it replaces the spec's. A harness can still
  print its environment into a run's events, so the docs say `[env]` is not
  for secrets: those are grants.
- **doctor** prints the load error the daemon would stop on, the declared
  names, and a warning when `DOCKER_HOST` is not declared while docker's
  current context names a unix socket that `/var/run/docker.sock` does not
  lead to — read from docker's own files, never by running docker. Docker
  Desktop links the default socket to its own and gets no warning; colima
  does not. It changes no exit code and nothing in `--json`.

### Claude's own settings.json

Measured on Claude Code 2.1.296 (2026-10-12), with a scratch
`CLAUDE_CONFIG_DIR` whose `settings.json` held only an `env` block, one
`claude -p` turn on haiku: a variable set there reached the turn's Bash call.
`BASH_MAX_TIMEOUT_MS` set beside it did not appear in the Bash call's
environment — Claude keeps its own settings out of the shell — and Claude
reads it from its own process environment (`process.env.BASH_MAX_TIMEOUT_MS`,
in that version's code) each time it bounds a Bash call, which is where
`settings.json` puts it. So a Claude setting has two routes: the `env` block
of `~/.claude/settings.json`, which every account's home links ([0076](0076-a-token-accounts-home-carries-its-login-and-every-account-shares-the-machines-plugins.md)), or
`[env]`, which reaches every harness. `CLAUDE_CODE_*` settings can only take
the first, since the scrub removes them. A real 30-minute command was not run.

## Considered options

**`[run] env`, as the ticket had it.** Reaches runs and not the probe or the
login check, so a docker probe and a run could disagree about which Docker
the runner has — the disagreement 0045 removed for host tools.

**Refuse every name a hub grant could set.** Grants may use any name but
`PATH`, `HOME`, `LD_*`, `DYLD_*` and the account list (0038), so that would
refuse nearly everything. The collision is the real conflict, and it is
refused where it happens.

**Drop a refused name and start anyway.** The owner would see runs fail as
before and nothing to say the declaration was ignored.

**`yad service install --env`.** A second place the same variables could
live, captured once and invisible to `yad config apply`. Left out.

**Per-harness tables.** A harness's own settings file already serves one
harness; `[env]` is for what every child needs.
