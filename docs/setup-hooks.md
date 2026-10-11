# Setup hooks

A run that checks out a git repository gets a fresh worktree of it, on a branch
of its own. A worktree is a clean checkout: no `.env`, no installed
dependencies, no database, no ports picked. The repository knows how to turn
that into something a harness can work in, so YAD asks it to. If the worktree
holds an executable file at `.worktree/setup`, YAD runs it before the harness
starts.

The hook is optional. A repository without one still runs, and the run's events
say there was no hook. Its counterpart, `.worktree/teardown`, runs when the
worktree is reclaimed ([Teardown](#teardown)).

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
| `WT_BRANCH` | the branch the run works on. Unless the hub names one, `yad/<connection>/<session>` — or `yad/<connection>.<n>/<session>` (n from 1) when an earlier hub connected under the same name used the same session id ([0079](decisions/0079-a-reused-connection-name-retires-the-earlier-registrations-rows.md)) |
| `WT_SLUG` | `WT_BRANCH` with every `/` replaced by `-` |
| `WT_REPO` | the repository's name, taken from its URL |
| `WT_SLOT` | a small integer from 1, unique among this repository's live worktrees on this runner. It is freed when the session's workdir is reclaimed, after the [teardown](#teardown) has finished, and reused |

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
reported. Whatever the runner's owner declared in config.toml's `[env]` — a
`DOCKER_HOST` for a Docker on colima, say — is there too, as it is for the
harness ([install.md](install.md#run-it-as-a-service)), so a hook needs no
override of its own for the machine it runs on.

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

## Teardown

What setup starts outlives the worktree unless something stops it: a
`docker compose up` keeps its containers, ports and named volumes after the
directory is gone, and the next worktree given the same `WT_SLOT` collides with
them. If the worktree holds an executable file at `.worktree/teardown`, YAD
runs it when the session's workdir is reclaimed — on the hub's
`close_session`, the owner's `yad sessions close`, the idle TTL or disk
pressure — before it removes the worktree and frees the slot
([0080](decisions/0080-reclaiming-a-workdir-runs-its-teardown-hook-first.md)).

- It is found and run as setup is: the file in the worktree, never one it
  links to outside the checkout, with the worktree as its working directory,
  as the runner's own user, in the same environment. The file that runs is
  the one the session left there, which the harness may have changed.
- It gets the same variables, with the values setup was given. `WT_BRANCH`
  is the branch the run started on, even if the harness has switched branch
  since, so a compose project named from `WT_SLUG` is the one setup made.
- It runs only in a worktree a run got as far as, whose session still holds
  its slot, whether or not setup succeeded. A path source never runs one.
- It never fails anything. A teardown that exits non-zero, runs past
  `[workdirs] teardown_timeout` (2 minutes by default) or will not start is a
  warning in the runner's log and `yad status`, and in the recent errors every
  hub reads in the runner's health, and the worktree is removed and the slot
  freed anyway. What the hook printed is not kept: the warning gives its exit
  status. The runner's collector waits for it before going on to the next
  workdir, so keep it to stopping what is running.
- **It must be safe to run twice.** A runner stopped while a teardown runs
  kills it, keeps the worktree, and runs the teardown again after it starts.
  Stop what is there and ignore what is not:

```sh
#!/bin/sh
set -u
: "${WT_ROOT:=$(git rev-parse --show-toplevel)}"
: "${WT_SLOT:=1}"
cd "$WT_ROOT"
docker compose -p "app-$WT_SLOT" down --volumes --remove-orphans || true
```

## Branches shared between sessions

git lets one worktree have a branch checked out at a time. A branch the hub
names is one a later session may name too: one run builds it, another verifies
or merges it. So when a run ends with that branch **pushed and clean** — no
change to a tracked file, and every commit on `origin/<branch>` — the session
lets go of it: its worktree is detached at the same commit, and nothing in it
changes. Another session naming the branch can then check it out
([0082](decisions/0082-a-session-lets-go-of-a-branch-it-left-pushed-and-clean.md)).

- **Untracked files do not count.** They stay in the worktree whatever becomes
  of the branch.
- **A push to a URL does not count as pushed.** Only a push to `origin` moves
  `origin/<branch>` in the runner's cache, so push to `origin`.
- **A session's own branch, named after it when the hub names none, is never let go of**, and a
  run that parks on a usage limit keeps its branch: it comes back to the tree
  it left.
- **The session's next run takes the branch back.** The cache is fetched, the
  branch is fast-forwarded to origin's when origin's has moved on, and the run
  starts on it — on the other session's commits too. Its status events say how
  the branch moved while the session was away, or that it was rewritten or has
  diverged from origin's.
- **A branch still held** — by a session with a run going, or one that left
  uncommitted changes or unpushed commits on it — fails the new run with
  `source_failed`, naming that session when it is the same hub's. Push and
  commit from that session, or close it, and send the run again.

The hook does not run again when a session takes its branch back: the worktree
is the one it set up. Two sessions' worktrees can hold the same branch, one
after the other, so name anything they must not share — containers, databases,
ports — after `WT_SLOT`, never `WT_BRANCH` or `WT_SLUG`.

## What it is not

A hook is the repository's own code, run with everything the runner's user can
reach. YAD runs it the way it runs the harness: it does not sandbox it. What
that means for the machine you give a runner is in
[run-it-safely.md](run-it-safely.md).
