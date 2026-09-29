---
date: 2026-09-29
---

# A fork is a new session opened from another session's conversation

A hub can ask for a run that continues a *copy* of a session's conversation,
leaving the session it copies untouched: yashiki's `sit` asks a side question
of a conversation that goes on without it. Both harnesses do this themselves —
Claude Code's `--resume <id> --fork-session`, Codex's `thread/fork` over the
app-server — and decision 0007 says the harness's transcript is the
conversation and YAD maps sessions rather than wrapping them. So a fork is
the harness's, and the protocol only has to say which conversation to copy.
DEV-48.

## Decision

**A fork is a new session.** `SessionRef` gains one optional field,
`fork_from`: the id of another session, allowed only on a run with
`session.new: true` and never naming the session itself. The run opens its
session as every new session opens — a hub-chosen id, a workdir of its own
built from the run's own sources, bound by the claim to the runner that
takes it — except that its conversation starts as the harness's copy of
`fork_from`'s, as far as the harness has written it. From then on it is a
session like any other: its later runs carry no `fork_from` and resume its own
conversation, and the session forked goes on resuming its own. Nothing in the
protocol distinguishes the two sessions again.

**The feature `fork` gates it.** A runner advertises `fork` in
`protocol_features`, and a hub offers a run carrying `fork_from` only to a
runner advertising it — ARCHITECTURE.md §2's rule, and `run/gated-features`
in the conformance suite now fails a hub that offers one to a runner
advertising nothing. It is one feature for the runner, not one per harness,
because every first-class adapter forks on every pinned harness version
(measured below) — a Claude without `--fork-session` is not driven at all,
and a Codex whose protocol drifted from the pinned ones is driven with a
warning, for forks as for every other method (decision 0037); a test holds `capability.Features()` and
`adapter.Forks` together, as it does for `effort`, and the runner refuses a
fork for an adapter that cannot (class `refused`) rather than start the
conversation empty.

**A fork happens where the transcript is.** A hub offers the run only to the
runner that holds `fork_from` — the session it bound to that runner — and the
runner checks it again at the claim: a session it does not hold *for this
hub's connection* (session ids are per connection, so a hub can never name
another hub's conversation), one of another harness, or one closed or closing
is refused, class `refused`, with the reason. The source's native id is read
when the fork's harness starts, not at the claim, so the copy is of the
conversation as it stands then. A source whose harness has not yet given it
a native id — its first run still preparing — has nothing to copy, and the
run ends `resume_rejected`, as a resume of nothing does (decision 0031); so
does a harness that has an id and no transcript behind it.

**The forked session is not locked.** A fork may run while a run in the
session it forks is live, which is what `sit` wants: a turn in progress
contributes whatever the harness has written of it. The source's one-live-run
rule is untouched, because the fork's run belongs to another session.

**A fork that did not take is a mismatch.** YAD chooses a Claude fork's
native id with `--session-id` beside `--resume --fork-session`, as it does for
a new session, so the id Claude echoes is checked as always; an echo of the
forked session's id means Claude ran the original and would write into it, and
the turn is stopped at its first frame with `session_mismatch`. Codex's
`thread/fork` answers with the new thread's id; an answer naming the forked
thread is the same fault, and no turn starts.

## yad hub

`yad hub submit --fork <session>` (and `SessionChoice.fork_from` in the
service API) queues a fork. The hub refuses at submit a fork that could never
be offered: `404` for a session it does not have, `409` for one closed or
closing, of another harness, bound to no runner yet, or on a runner not
advertising `fork`. The hub's session row keeps `fork_from`, because the run
that opens the session is decided at offer (decision 0047): if the fork's
first run never binds it, its next run is the one sent with `new: true`, and
it carries the same `fork_from`. `OfferCandidates` offers an unbound fork only
to the forked session's runner, and a runner that goes away — deregistered or
silent past abandon-after — closes the unbound forks of its sessions with its
own, since no other runner could ever open them.

**Nor does a fork wait on a source that can no longer be forked** (DEV-151,
decided 2026-09-29 by the manager of that night's run, owner asleep; cheap to
reverse). A source bound at submit can later be unbound — the claim that bound
it withdrawn, and the session deleted with it on the runner (decision 0061) —
or closed, by the hub's word or its runner's. A fork of it would then be
offered to no runner, or to one that refuses it, and a queued run holds no
lease for the sweep to end. So at that moment `yad hub` closes every fork of
the source that no claim has bound and fails the runs waiting in them, with a
reason saying the source has no conversation to copy: the `resume_rejected`
end above, in the hub's words, since a run the hub ends carries no class. A
close asked for is enough, since the runner refuses a fork of a session it is
closing. Waiting for the source to be bound again was weighed and rejected: a
run that binds it opens a new conversation, not the one the fork asked for,
and the run may never come.

## Measured

- **Claude Code 2.1.284** (`claude --help`): `--fork-session`, "When
  resuming, create a new session ID instead of reusing the original". Run
  with a session that does not exist, `--resume X --fork-session
  --session-id Y` is accepted and fails `No conversation found with session
  ID: X` under Y, spending nothing; `--resume X --session-id Y` without
  `--fork-session` is refused ("--session-id can only be used with --continue
  or --resume if --fork-session is also specified"). The flags probe now asks
  for `--fork-session` beside `--system-prompt-snapshot`; every release with
  the second has the first, so no Claude that could run before is refused.
- **Codex 0.147.0 and 0.157.1**: both schema bundles in
  `internal/adapter/codex/testdata/` have `thread/fork` with `threadId`,
  `cwd`, `model`, `approvalPolicy`, `sandbox` and `developerInstructions`, as
  `thread/resume` has, and a `ThreadForkResponse` carrying the new `thread`.
  The adapter's pinned surface now includes both, so each release's hash
  moved. On 0.157.1, `thread/fork` of a thread with no rollout answers `no
  rollout found for thread id …`, the words of a resume of nothing.
- **Recorded**, one short turn each (`fork`, `fork-missing` under
  `claude-2.1.284/` and `codex-0.157.1/`): a first turn is told a word, the
  fork is asked for it and answers it in a new session, and the recorder
  compares the forked session's transcript (Claude's `projects/*/<id>.jsonl`,
  Codex's rollout) before and after the fork, byte for byte — unchanged, on
  both. Codex 0.147.0 was not installed to record against; its `thread/fork`
  is the same shape in its schema and its hash covers it.

Not measured: whether Codex reads a fork's `developerInstructions` before a
compaction, as decision 0050 found it does not on a resume. A fork is a
resume into a new thread, so the adapter treats it as one and also injects the
run's context before the turn; at worst the model reads it twice.

## Considered options

**`fork: true` on a continuing run**, forking the session the run names.
Smaller, and wrong on every other axis: the fork would need an id the hub
never chose, and a session the protocol never named, bound to nothing a later
run could address.

**A `fork_session` control** creating the session without a run. A second way
to open a session, one no other part of the protocol knows, for a thing that
is only ever wanted with a turn in it.

**Sharing the forked session's workdir.** The conversation would find the
files it remembers, and two conversations would edit one tree at once —
decision 0032 gives a workdir to one session. A hub that wants the fork to see
the same code names the same repository in the fork's sources, on a branch of
its own cut from the source's (`base`).

**Copying the workdir.** Costly, and it copies build output, caches and a
worktree git will not share with its original. Left to the hub's sources, as
above.

**A feature per harness** (`fork:claude`). Needed only if a drivable harness
could not fork; none can't, and one added later without it is refused at the
run rather than advertised.
