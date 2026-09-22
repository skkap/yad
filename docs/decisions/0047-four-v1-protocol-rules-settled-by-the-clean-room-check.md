---
date: 2026-09-22
---

# Four v1 protocol rules settled by the clean-room check

DEV-110 had a fresh agent write a hub from HUB.md and the OpenAPI document
alone, and read where it had to guess. Most of its questions `yad hub` already
answered and HUB.md now says so. Four it did not: HUB.md carried each as a
marked open point, and each became a ticket — DEV-115 to DEV-118. The owner
decided all four on 2026-09-22. They are recorded together because they are
one kind of thing: a rule a hub has to know and could not read anywhere.

## Which sync fields a hub may refuse a sync over (DEV-117)

**Decision.** `health.load`, `disk_free_bytes`, `spool_depth` and
`outbox_depth` are optional in `protocol/v1` — `omitempty`, and out of the
document's required list. A hub may refuse a sync only over what routing reads:
`runner_id`, `fingerprint`, `health.free_capacity.total`, and each listed run's
`run_id` and `state`, plus the required fields inside a part the runner chose to
send (a capability document, a closed session, a harness's health). HUB.md §3
says so, and conformance checks it (`sync/dashboard-health-optional`).

**Why.** The four are for dashboards, and a refused sync renews no lease: a hub
strict about one of them turns a runner build that leaves it out into every
held run lost. The document said they were required, so a hub generated from
it was strict by default.

**What it costs.** `omitempty` on a number drops a real zero, so an empty spool
is now sent as no `spool_depth` at all: a hub generated from the earlier
document would refuse a yad runner's ordinary sync. Nothing had been released,
so no such hub exists; after a release this would have been a v2 change.
oasdiff rates the v1 document's change non-breaking, since it loosens a
request. The service API (`protocol/hubapi`) re-serves a runner's health, and
there the same loosening would have been a breaking change to a response, so
it has its own `Health` type that still answers all four, as 0 where the
runner left one out; a test keeps it carrying every field of v1's.

**Considered.** Leaving the fields required and calling a build that omits one
broken — the cost of a missed field is every run on the machine, out of
proportion to a dashboard. Keeping them required and saying in HUB.md that a
hub should tolerate their absence — a sentence against the document, when the
document is what hubs generate their validators from.
