---
date: 2026-09-19
---

# A cancel is repeated until the run ends, and an answer that landed first stands

`yad hub cancel`, `interrupt` and `steer` (DEV-8) reach a runner as controls in
its sync responses, and the runner acts on them through the cancel ladder of
ARCHITECTURE.md §3. Four things about that were not settled by the protocol as
written.

**Controls are not acknowledged, so a cancel is repeated.** A sync response can
be lost — the hub answered, the runner never read it — and nothing in the
protocol says a control arrived. So a hub sends `cancel` and `interrupt` in
every response to a sync that lists the run, until the run ends, and the runner
acts on the first and ignores the rest. Both are idempotent by nature. A
`steer` is not — the harness would read the same text twice — so it is sent in
one response and forgotten: a steer can be lost with a response, and the
person who sent it sees no `steered` status in the run's events and can send it
again. `yad hub` keeps the requests in a `run_controls` table; the service
API's view of a run shows `cancel_requested_at` until it ends, and wakes a
long-polling watcher when a cancel is asked for.

**A run that has not started ends without a round trip, and without a
process.** On the hub, a queued run, or one offered and not yet claimed, is
cancelled on the spot; an offered run's runner hears `cancel` for it at its
next sync and withdraws it, as decision
[0019](0019-a-run-starts-once-its-claim-is-acknowledged.md) already provides.
On the runner, a claimed run waiting on its `start_at`, or still preparing, is
cancelled with nothing spawned. An interrupt that arrives before the harness is
up does the same: there is no turn to end, and ending the run keeps the session
exactly as an interrupt would. The service API refuses interrupt and steer for
a run no runner holds (409, with "cancel it instead" or "put it in the brief").

**The answer that landed first stands.** A harness can finish — successfully,
or with a usage limit, a missing session, a prompt that is too long — while a
cancel or an interrupt is on its way to it. Decision
[0021](0021-claude-runs-end-at-the-last-result.md) already kept a success that
came first; this extends it to an error. Reporting such a run `cancelled`
would hide the one thing the hub needs to act on: that the account is limited
until a given time, or that the session is gone. For Claude, "first" is read
from the stream: Claude acknowledges an interrupt with a `control_response`
before it aborts, so a final result that arrived before that acknowledgement,
and is not itself an abort (`terminal_reason: aborted_*`), is the turn's own
answer. A run the ladder had to kill has no answer of its own, and is
`cancelled`. A watchdog is different, and unchanged: its verdict, `timed_out`,
wins over whatever the stopped harness said last.

**Interrupt only asks; cancel makes sure.** An interrupt is the ladder's first
rung alone. A harness that ignores it carries on, and the run ends however it
would have — which is what the inactivity watchdog is for if it never does.
Cancel climbs the whole ladder, and so does a watchdog: the SIGTERM rung, which
the watchdog skipped before, gives a harness's own children the chance to exit
cleanly before SIGKILL. `cancel_latency_ms` is measured from the moment the
runner received the control to the end of the turn, for an interrupt as for a
cancel; a run stopped before it started reports the time to its result.

## Considered options

**An acknowledgement field for controls in the sync request.** Exact-once
delivery for steer too, and a new field every hub must implement and every
runner must track, for the one kind that is not idempotent. Repeating the
idempotent ones is free; a lost steer is visible and cheap to resend. It can be
added within v1 if steers turn out to matter more than that.

**The cancel always wins.** Simpler to say, and it throws away a usage limit's
reset time, the one fact the hub needs to decide where the next run goes — the
same loss decision 0021 refused for a success.

**Interrupt escalates like cancel.** Then the two would differ only in name,
and a person who wanted the turn to stop politely would get SIGKILL on a harness
that was merely slow to answer.
