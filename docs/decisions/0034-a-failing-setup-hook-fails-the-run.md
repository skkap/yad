---
date: 2026-09-19
---

# A failing setup hook fails the run; slots are per repository and start at 1

A repository's `.worktree/setup` is what makes a fresh checkout runnable —
dependencies, `.env` files, a database on the worktree's own ports. YAD runs
it as `gpiwt` does (DEV-17), with `WT_ROOT`, `WT_MAIN`, `WT_BRANCH`, `WT_SLUG`,
`WT_REPO` and `WT_SLOT`, and had to decide what a failure means.

**A hook that fails fails the run** in `preparing`, with error class
`setup_failed` and the hook's last line in the message. The contract says a
hook exits non-zero only when the checkout needs a human; a harness started in
it anyway would spend the run on a broken environment and report whatever it
made of that. `gpiwt` only warns because a person is watching; nobody watches a
runner. A hook past the owner's `[workdirs] setup_timeout` (default 15 min) is
killed with its process group and fails the same way. The worktree stays, and
the session's next run runs the hook again: it runs until it has succeeded
once in a worktree, marked in the worktree's own git directory so the tree
stays clean, and never after.

**Its output becomes events.** A `tool_call` named `setup hook`, then a
`tool_result` holding the last 8 KiB of what it printed, stdout and stderr in
the order written — the closing lines are the ones that say what it did and
did not do. No hook, or one that is not executable, is a `status` event, and
the run proceeds on a clean checkout.

**Only the committed hook.** `gpiwt` also looks in the main checkout and the
owner's hooks directory; a runner has neither, and a hook that is really a
symlink out of the checkout is not run. A hook is the repository's code, run as
the runner's user like the harness after it — no more trusted, and no less.
It gets no grant: grants are for the harness
([0009](0009-machine-owns-credentials-hubs-grant-per-run.md)). A `path` source
runs no hook: it is the owner's own directory, already set up.

**`WT_MAIN` is the bare cache.** It holds every branch and the remote, which is
what a hook reads it for; the untracked values a hook reads from a person's
main checkout do not exist on a runner.

**Slots.** `WT_SLOT` is the smallest free positive integer among one
repository's live worktrees on this runner, kept by the session for as long as
it keeps its worktree, stored in the runner's `slots` table, and freed when the
workdir is reclaimed (`workdir.Manager.Reclaim`, which DEV-18 calls). Slot 0 is
a main checkout's in the `gpiwt` contract, so a runner starts at 1.

## Considered options

**Proceed with a warning**, as `gpiwt` does — right for a person at a
terminal, wrong for a run nobody reads until it has finished.
**Run the hook on every run.** Hooks are idempotent by contract, but a slow one
would tax every turn of a long session.

## Consequences

A runner on a machine that also has `gpiwt` worktrees of the same repository
allocates slots from 1 too, and their ports collide. 0015's advice — a runner
on a dedicated machine, VM or container — avoids it; an owner who ignores it
sees the hook fail and the run say so.
