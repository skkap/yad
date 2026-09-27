# Setup hooks

A run that checks out a git repository gets a fresh worktree of it, on a branch
of its own. A worktree is a clean checkout: no `.env`, no installed
dependencies, no database, no ports picked. The repository knows how to turn
that into something a harness can work in, so YAD asks it to. If the worktree
holds an executable file at `.worktree/setup`, YAD runs it before the harness
starts.

The hook is optional. A repository without one still runs, and the run's events
say there was no hook.

## When it runs

- In the run's `preparing` state, after the worktree is checked out and before
  the harness starts.
- With the worktree as its working directory, as the runner's own user, like
  the harness after it.
- Until it has succeeded once in that worktree. A session's later runs reuse
  the worktree and skip the hook. The mark that says it succeeded is kept in
  the worktree's git directory, not in the tree, so the harness sees a clean
  checkout.
- Only the committed `.worktree/setup`. A hook that is a link to a file outside
  the repository is not run.

## What it gets

| Variable | Value |
|---|---|
| `WT_ROOT` | absolute path of the worktree, which is also the working directory |
| `WT_MAIN` | absolute path of the runner's bare cache of the repository: every branch and the remote, but no working tree and no untracked files |
| `WT_BRANCH` | the branch the run works on. Unless the hub names one, `yad/<connection>/<session>` |
| `WT_SLUG` | `WT_BRANCH` with every `/` replaced by `-` |
| `WT_REPO` | the repository's name, taken from its URL |
| `WT_SLOT` | a small integer from 1, unique among this repository's live worktrees on this runner. It is freed when the session's workdir is reclaimed, and reused |

`WT_SLOT` is the one a hook cannot work out for itself. Use it for anything two
worktrees of one repository must not share: ports, container and compose
project names, database names. A common pattern is a block of ten ports per
slot:

```sh
APP_PORT=$((25500 + WT_SLOT * 10))
DB_PORT=$((25500 + WT_SLOT * 10 + 1))
```

The environment also carries the git settings that stop git from prompting
(`GIT_TERMINAL_PROMPT=0` and the askpass variables cleared), because nobody is
there to answer. `PATH` puts the runner's `git`, `gh` and `docker` first, so a
`docker compose up` in the hook reaches the same Docker that `yad doctor`
reported.

Give every variable a fallback, so the hook can still be run by hand:

```sh
#!/bin/sh
set -eu
: "${WT_ROOT:=$(git rev-parse --show-toplevel)}"
: "${WT_SLOT:=1}"
cd "$WT_ROOT"
cp -n .env.example .env
npm ci
```

## When it fails

A hook that exits non-zero, or runs past `[workdirs] setup_timeout` in
`config.toml` (15 minutes by default), fails the run with the error class
`setup_failed` and its last line of output. The worktree is kept, and the
session's next run tries the hook again
([0034](decisions/0034-a-failing-setup-hook-fails-the-run.md)).

The hook's combined output reaches the hub as one `tool_call` and
`tool_result` pair, capped at 8 KiB from the end.

## What it is not

A hook is the repository's own code, run with everything the runner's user can
reach. YAD runs it the way it runs the harness: it does not sandbox it. What
that means for the machine you give a runner is in
[run-it-safely.md](run-it-safely.md).
