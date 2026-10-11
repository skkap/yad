---
date: 2026-10-12
---

# A session lets go of a branch it left pushed and clean

[0033](0033-sources-reach-only-what-the-owner-allows.md) checks a git source
out as a worktree of the runner's bare cache, on the branch the run names. git
lets one worktree have a branch checked out at a time, so while a session's
worktree is on a branch, a new session naming that branch failed to start, with
git's own line: `fatal: 'X' is already used by worktree at '<data>/workdirs/<connection>/<session>'`.
That reached the hub as `source_failed` with no next action and a path naming
another connection's session, against
[0064](0064-a-runs-error-may-name-a-path-on-the-runner.md) and
[0065](0065-a-fork-is-a-new-session-opened-from-another-sessions-conversation.md).
Outsourcing makes it the normal case: one run builds a branch, another verifies
or merges it, and sessions stay open for days (the idle TTL is 14 days).
JapanLaw #1's integrator had to work around branches "left checked out in
worktrees". DEV-189.

## Decision

The owner's (2026-10-12): **a session lets go of a branch when a run of it
ends, and takes it back at its next run's start, but only when it left the
branch pushed and clean.** Pushed and clean is no change to a tracked file, and
HEAD contained in `origin/<branch>` as the cache last saw it; untracked files
do not count, since they stay in the worktree whatever becomes of the branch.
Otherwise the branch stays where it is, and a new session naming it gets an
error naming the session that holds it.

What building it settled:

- **Only at a run's terminal end, never at a park.** `workdir.Manager.Settle`
  is called from the executor's `finishWith`, before the result's
  transaction — once the hub has the result, the session's next run, or
  another session's, may come at once — and for each run a restart reports
  lost, before its result too, so a runner that stops in between tries again.
  A parked run comes back to the tree it left, branch and all, and one ended
  while parked — past its `max_wait`, or cancelled — is settled only after
  the transaction that checks it is still waiting has committed: a sync may
  be resuming it at that moment, with a harness at work on the branch. By
  then its result may have been delivered and the session's next run
  claimed, so that settle keeps the branch when the store holds a run of the
  session, asked under the cache's lock (`SettleUnless`): that run's
  preparation takes the same lock, so it has either finished and is left
  alone, or will find the release mark.
- **Only the branch the hub named.** A session's own
  `yad/<connection>/<session>` is never let go of: no other session names it.
  Sessions naming no branch behave exactly as before, with no new fetch. The
  named branch is recorded in the worktree's git directory (`yad-branch`) at
  every run, so a worktree an earlier version made is marked by its first run
  under this one.
- **Letting go is a detach at the same commit.** Not a file in the worktree
  changes. The commits stay on `refs/heads/<branch>`, since only HEAD moves.
  A `yad-detached` mark holding the branch's name is written first, so a
  runner that stops between the two comes back to a mark beside a HEAD still
  on the branch, and drops it. A detached HEAD with no mark is the harness's
  own doing and is left alone.
- **Taking it back** fetches the cache first. A failed fetch is a status event,
  not a failure: a continuing run that names no sources has no credential for
  a URL that needed one ([0068](0068-a-credential-in-a-source-url-is-the-runs-alone.md)).
  Then, under the cache's lock:
  - while another worktree has the branch checked out, the run fails
    `source_failed`, naming the holder, and the mark stays for the next run;
  - when the cache's branch is behind origin's, it is fast-forwarded with an
    `update-ref` guarded by its old value, and the run is told
    (`<branch> fast-forwarded from <old> to origin/<branch> at <new>`);
  - when the two have diverged, the branch is taken as it stands here, and the
    run is told how many commits each side has;
  - when the branch no longer contains the commit the session left it at —
    another session rewrote it — the session goes with the branch, and the
    run is told which commit it no longer has;
  - a `git switch` refused because a file the session left untracked has since
    been committed on the branch fails the run naming the files, not with
    git's message, which quotes the workdir's path.
- **A new session naming a branch the cache already has** takes it the same
  way: refused naming the holder, and fast-forwarded when origin's has moved on.
  Before, it was checked out "as it stands", behind origin, and the session's
  first push was refused.
- **The holder is named only to its own hub.** It is found with `git worktree
  list --porcelain` and the store's `SessionByWorkdir`. To the same connection
  the error says `session <id>` and why it kept the branch — uncommitted
  changes, commits never pushed or not on origin, or a run going. To another
  connection it says "a session of another hub on this runner", and no id or
  path. The class stays `source_failed`, which a hub retries; the message
  carries the next step.
- **Forks** ([0065](0065-a-fork-is-a-new-session-opened-from-another-sessions-conversation.md))
  follow the same rule. HUB.md still advises a fork to name the same
  repository on a branch of its own.
- **`PushedAndClean`** is the exported check, for a session handed to another
  runner (DEV-43) to reuse.

## Consequences

- A push to a URL rather than to `origin` does not move `origin/<branch>` in
  the cache, so the session reads as unpushed and keeps the branch. That is
  the safe side, and the error says so.
- The continuing harness is not told that its branch moved under it; the run's
  status events say so (`old..new`), for the hub to relay.
- Every run ending in a session that names a branch runs `git status` on its
  worktree, which on a large tree delays the result by seconds. Every
  continuing run whose branch was let go of fetches, best effort.
- Letting go never hands unpushed work to another session. A session closed
  with commits it never pushed still leaves them on the cache's branch, as it
  did before this decision, and a later session naming that branch — of any
  hub on the runner — starts from them as they stand.

## Considered options

**A branch of the session's own, `yad/<connection>/<session>`, tracking the
named one.** Two sessions could never contend. But `git push origin X` from
it pushes the shared `refs/heads/X`, which another session may have moved: a
push that silently publishes someone else's commits.

**`git worktree add --force`, or `--ignore-other-worktrees`.** git allows it.
The worktree that held the branch then shows the other session's commit as a
staged reverse change, which its next run would read as its own work.

**Let go of every branch at run end, whatever its state.** Simpler, and a
session coming back to a branch another session rewrote would lose the work
only it had. Kept to pushed and clean, nothing is lost that is not on the
remote.

**A dedicated error class for a contended branch.** A hub can do nothing with
it that it cannot do with `source_failed` and a message: send the run again
once the holder's run has ended.
