---
date: 2026-09-22
---

# Runs use the host tools detection resolved

`YAD_GIT_PATH`, `YAD_GH_PATH` and `YAD_DOCKER_PATH` chose which binary
host-tool detection probed, and the capability document advertised that tool as
present and usable. Nothing that ran used the choice. yad's own git
(`internal/workdir`) was `exec.LookPath("git")`, and supervise strips every
`YAD_*` variable before starting a harness, so the harness's gh and docker came
from `PATH`. The override exists for a daemon whose `PATH` lacks the tool
([0044](0044-an-override-that-names-nothing-lets-path-decide.md)). On such a
daemon a hub routed git-source work to a runner whose workdir preparation then
could not find git (DEV-107).

## Decision

The manager's (2026-09-22): **a run uses the host tool detection resolved**,
through the same lookup (`hostool.Locate`, which is `internal/probe`'s), so
detection and use cannot disagree.

- **yad's own git** is `hostool.Locate("git")`, looked up again for every git
  command. An override edited or deleted while the daemon runs is what the next
  command uses, as it is what the next probe reports.
- **A run's children** — the harness and the repository's setup hook — find the
  resolved git, gh and docker first on `PATH`. The profile's data directory holds
  `host-tools/`, with a symlink named `git`, `gh` or `docker` for each tool that
  **only its override** finds, and that directory is prepended to the child's
  `PATH`. A link and not the override's directory, because the override's file
  need not be called `gh`: `/opt/gh-beta/bin/gh-2.99` is a fine thing to point
  `YAD_GH_PATH` at. A tool `PATH` resolves gets no link, because `PATH` already
  finds it; with no link at all, `PATH` is left as it was.
- The directory is reconciled with detection each time a run starts a harness or
  a hook: a stat or two per tool. A link is replaced by rename, so a run already
  going never finds it missing. The links point at the override's absolute path.
  They and the `PATH` that carries them stay on the machine, and no message
  quotes them.

A link that cannot be made fails the run as `prepare_failed` (a hook's as
`setup_failed`), with the detail in the log. Starting the harness without it
would run a different gh from the one the capability document advertised, and
that is the failure this decision exists to end.

## Rejected

- **Stop advertising a tool only an override finds.** The override is the
  documented way to give a GUI-launched daemon its tools. Advertising none of
  them would make it useless for exactly the machine it was added for.
- **Prepend the override's directory.** It finds nothing when the file has
  another name, and it puts everything else in that directory ahead of `PATH` —
  a directory like `/opt/homebrew/bin` holds hundreds of binaries.
- **Pass `YAD_<ID>_PATH` through to the harness.** No harness reads it. Its
  shell commands run `gh`, by name.
