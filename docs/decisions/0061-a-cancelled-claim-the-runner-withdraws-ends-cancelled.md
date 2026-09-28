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

No protocol shape changes. Conformance cannot check it — only a hub's own API
asks for a cancel — so it joins the controls on the suite's not-checked list.

## Considered options

**Leave it `lost`.** It is what the hub observes, and it is rare: it needs an
answer lost at the one moment a cancel is asked for. But the submitter asked
for the run to stop, it never started, and `lost` tells them something failed.

**Cancelled only when a sync reports the withdrawal**, lost on a lapse. Truer
to what the hub knows, and it makes the ending depend on whether the runner
happened to sync again — the same withdrawal would read two ways.
