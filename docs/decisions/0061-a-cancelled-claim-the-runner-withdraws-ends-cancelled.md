---
date: 2026-09-29
---

# A cancelled claim the runner withdraws ends cancelled

A runner starts a claimed run only once a sync listing it has been answered
without a `cancel` for it, and withdraws the claim — never started, no result
owed — when the answer carries one ([0019](0019-a-run-starts-once-its-claim-is-acknowledged.md)).
One path through that was left to the lease (DEV-114, noted by DEV-113): the
answer that acknowledged the claim is lost in transit, so the hub holds the run
as claimed while the runner is still waiting to hear so, and a cancel asked
for then reaches the runner first. The runner withdraws, as 0019 says, and
stops listing the run. `yad hub` did nothing until the lease lapsed, and then
recorded the run `lost` — a run that ended exactly as someone asked, shown to
them as a failure.

## Decision

The owner's (2026-09-29).

**A claim the hub has asked to cancel ends `cancelled` once the runner no
longer holds it, whether a sync leaves it out or its lease lapses.** A runner
lists every run it holds until the hub has taken its result, so a claimed run
a sync leaves out with no result is one it withdrew; `yad hub` cancels it at
that sync, with a reason saying the runner withdrew its claim. When no sync
comes, the lapse cannot tell a withdrawal from a runner gone silent, and the
end is `cancelled` all the same: nothing said the run started, and the end is
the one asked for. HUB.md §3 states the rule for every hub.

It is narrow on purpose. Only a run the hub holds as `claimed` — never one a
sync has reported `preparing` or later, which owes a result and is `lost` if
none comes — and only with a cancel asked for. A claim nobody asked to cancel
that a sync leaves out stays held until its lease lapses, and is lost, as
before. The rule holds too when a runner deregisters holding such a claim, so
the three ways a withdrawn claim can reach a hub agree.

**The session that claim bound goes back to unbound, at the sync that leaves
the claim out** (DEV-143). The runner deletes the session a withdrawn claim
opened, so a hub that keeps it bound sends the next run in it as continuing,
to that runner alone, which refuses it as a session it does not hold. `yad hub`
keeps which run's claim bound each session (`sessions.bound_by_run`) and
unbinds the session only when the withdrawn claim is that run. A later claim
in the session came after a run that ran, whose session the runner keeps
(DEV-77: a session with a transcript is closed, never unbound); a session with
a close asked for is closed on the runner rather than deleted, and the hub is
waiting for that report; a lapsed lease cannot tell a withdrawal from silence,
and a departed runner's sessions close. Each of those stays bound.

**So does a lapsed claim the runner comes back listing as `claimed`**
(DEV-148): the answer acknowledging it was lost, and then the runner was
silent past the lease, so the hub ended the run — `lost`, or `cancelled`
under the rule above — and answers the listing with a cancel. A claim still
listed as `claimed` is one no answer acknowledged, and the hub will never
send one now, so the runner withdraws it, and the session it opened, at
whichever cancel it hears first. `yad hub` unbinds the session at that
answer, under the same rules as the withdrawal above, rather than waiting
for a sync to leave the claim out, which would add nothing. A run listed
`preparing` or later started, and keeps its session; so does a run with a
start moment, whose acknowledged claim the runner lists as `claimed` until
the moment, where the listing cannot tell a lost answer from a kept session.
The answer carrying such a cancel offers nothing in the run's session: a
runner stops a run it is executing only after claiming the offers beside the
cancel, and refuses a second live run in a session.

No protocol shape changes. Conformance cannot check it — only a hub's own API
asks for a cancel — so it joins the controls on the suite's not-checked list.

## Considered options

**Leave it `lost`.** It is what the hub observes, and it is rare: it needs an
answer lost at the one moment a cancel is asked for. But the submitter asked
for the run to stop, it never started, and `lost` tells them something failed.

**Cancelled only when a sync reports the withdrawal**, lost on a lapse. Truer
to what the hub knows, and it makes the ending depend on whether the runner
happened to sync again — the same withdrawal would read two ways.
