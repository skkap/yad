---
date: 2026-10-12
---

# Reclaiming a workdir runs the repository's teardown hook first, and a failed one is only logged

YAD ran a repository's `.worktree/setup` when a worktree was made
([0034](0034-a-failing-setup-hook-fails-the-run.md)) and ran nothing when it
was reclaimed ([0035](0035-a-runner-reports-every-close-in-its-sync.md)). Every
Postgres or compose stack a run started stayed up after its worktree and slot
were gone, holding ports, memory and named volumes. The next session given
that slot derived the same ports and collided with the orphan. After
Zumino's first outsourcing night on Ashikaga (JapanLaw #1), ten Postgres
stacks and their volumes were removed by hand (YAD-187).

## Decision

**A reclaim runs `.worktree/teardown` in each worktree before removing it**,
whatever closed the session: `close_session`, `yad sessions close`, the idle
TTL, disk pressure or a removed connection. Then the worktree is removed, and
only once every worktree of the session is out are its slots freed. The slot
is therefore not given to another worktree until the teardown has finished.

**The same contract as setup.** The hook is found by the same rules — the
file in the worktree, executable, not a link out of the checkout — and started
the same way: the worktree as its working directory, no terminal, stdout and
stderr merged, the scrubbed environment with git's prompts off and the
runner's `git`, `gh` and `docker` first on `PATH`, as the runner's user, with
no grant. It gets the same variables. `WT_ROOT` is the worktree, `WT_MAIN`
the bare cache, and `WT_SLOT` the slot the session holds, read without
allocating one. `WT_BRANCH`, `WT_SLUG` and `WT_REPO` are the values setup was
given, recorded in the worktree's git directory (`yad-hook-env`) when it was
checked out. The branch is the one the run started on even if the harness
switched away from it, since a hook derives compose project names from it. A
worktree made before this decision has no record; it gets its current
branch and the repository's name read from its cache's.

**When it runs.** Only in a worktree whose add finished and whose session
holds a slot for the repository, whether or not setup succeeded: a setup that
failed half way may still have started something. Never for a path source,
which is the owner's own directory, nor for a half-made worktree no run worked
in.

**A failure is logged and the reclaim goes on.** A teardown that exits
non-zero, runs past `[workdirs] teardown_timeout` (default two minutes) or
will not start is a warning in the runner's log, and the worktree is removed
and the slot freed anyway. The warning's message is generic; the connection,
session, repository, path, slot and exit status go in its attributes. What
the hook printed goes nowhere: unlike setup's, which is the run's own event
stream, a teardown's output has no reader, and the log is not the place for
whatever a hook prints, a credential included. It reaches `yad status`
and each hub's `health.recent_errors` with no protocol change. Nobody is
waiting on a reclaim to read a richer error: the close was reported to the hub
before the sweep reclaims (0035), and `closed_sessions` has no field for it.

**At most once, but safe to repeat.** A mark in the worktree's git directory
(`yad-teardown-done`) is written after an attempt that finished, however it
ended. A reclaim retried after a failed removal does not run it again. A
reclaim cancelled while its teardown runs — the daemon stopping — kills the
hook, keeps the worktree and the slot, and the next sweep after the next start
runs the teardown again. A teardown must therefore be safe to run twice. A
runner that cannot link its tool overrides logs that and skips the teardown,
rather than running it against a `docker` other than the one `yad doctor`
reported.

**Outside the repository's lock**, as setup is: a slow teardown does not hold
up another session preparing a worktree of the same repository. Sweeps stay
one at a time on the collector, and a sweep now times out parked runs before
its closes, so a long teardown does not delay a run past its hub's
`max_wait`.

## Considered options

**Hold the slot after a failed teardown**, so its ports are not reused while
the stack might still be up. It would leak a slot for every broken teardown,
for ever, with no owner action that frees it, and the acceptance criteria
for YAD-187 ask for the workdir to go regardless. Deferred until Ashikaga
shows orphan stacks surviving a failed teardown.

**Report the outcome to the hub** in `closed_sessions`. It would need a new
protocol field. The close is reported before the reclaim, so it would also
mean delaying the report, which reverses 0035's timing for a value no hub
acts on.

**Run it under the repository's lock**, beside the removal. Simpler to reason
about, but a two-minute teardown would hold every preparation of that
repository for two minutes.

## Consequences

- A sweep takes longer: up to the teardown timeout for every worktree it
  reclaims, and reclaims behind it wait.
- A teardown killed at its timeout can leave a compose project half torn
  down. The warning says to check.
- The teardown that runs is the one the session left in its worktree, which
  the harness may have edited. A setup hook is the same (0034).
- A teardown reaches only the `docker` the daemon's environment and its
  overrides point at.
