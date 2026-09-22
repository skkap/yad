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

## Whether v1 may add a value to an enum (DEV-116)

**Decision.** Every enum the document declares — event kind, held run state,
result state, session close reason, control kind, and the rest — is closed for
all of v1. A value added to one arrives only behind a feature the receiving
side advertised: `hub_features` for what a runner sends, `protocol_features`
for what a hub sends, as new controls already worked. HUB.md §2 and §11 say
so, and so does the description of each of the five in the document.

**Why.** `yad hub` validates bodies against the document and refuses a value
outside an enum as `invalid`, and a runner drops an events batch refused as
`invalid` — so a new event kind sent to an older hub would lose the whole
batch, events of the known kinds with it. A hub generated from the document is
strict the same way by default. Making every hub tolerant instead would mean
each one validating these fields as free strings, against its own generated
types, to protect a value nobody has proposed yet.

**How it holds today.** v1 defines no hub feature, so there is nothing a yad
runner may send beyond the listed values, and it keeps no `hub_features` (the
register answer's are dropped at connect). The first value gated on one needs
the runner to keep them per connection first. `TestTheV1EnumsAreClosed` pins
each set as v1 shipped it, and fails on a new value with this rule in its
message; `TestEveryEnumIsPinned` fails on an enum it does not cover. The gated
values that do exist today go the other way — `steer`, `interrupt`, `drain`,
`close_session`, a `start_at` still ahead — and were already tested in
`internal/hub/features_test.go` and by conformance.

**Considered.** A tolerant hub that stores an event of an unknown kind — each
hub would have to loosen its generated validation for these fields, and a
runner still could not tell an older strict hub from a tolerant one. Declaring
them closed with no way to grow — the feature route costs nothing until it is
used, and new controls already take it.

## How long the runner a run was offered to may report its result (DEV-115)

**Decision.** A result from the runner a run is offered to is accepted while
the offer is open: from the answer that offered it until a sync takes the
offer back by leaving the run out, or the offer's lease lapses. After that it
is `403 not_holder`, which a runner drops. An offer made again reopens it.
HUB.md §6 says so, and conformance checks it
(`result/refusal-after-offer-taken-back`).

**Why.** This is how a runner refuses a run without claiming it (decision
0019), and it was the rule `yad hub` already kept; HUB.md named no time bound.
Once an offer is taken back the run may be offered to another runner, and a
wider door would let a runner fail a run another now holds. The cost is small
and bounded: a yad runner posts its refusal straight after the sync that
carried the offer, so it lands while the offer is open; a refusal whose first
attempt fails may be dropped, and the run is offered again and refused afresh.

**One fix it took.** `yad hub` requeues a lapsed offer at its next sweep — the
next sync, or the sweep timer — and until then still took a refusal for it.
It now reads the offer's lease when a result arrives, so lapsed is lapsed
whether or not the sweep has run.

**Considered.** The clean-room hub's guess — the last runner a run was offered
to, while the run sits queued and offered to no one else — keeps the door open
across a window in which the hub may offer the run to someone else at any
moment, so whether a late refusal lands would depend on queue order. A grace
period after the offer is taken back — a second timing every hub would have to
choose and no runner can learn.

## `session.new` for a session whose first run never bound it (DEV-118)

**Decision.** The hub decides `session.new` when it offers a run: `true` while
no claim has bound the session, `false` after. `yad hub` no longer sends the
flag its submitter set — that flag now decides only whether the hub creates
the session or finds it. And a runner restarting withdraws a claim the hub
never acknowledged together with the session it opened, as a live process
does on withdrawal (`settleSession`), while a claim the hub did acknowledge is
reported lost and keeps its session. HUB.md §8 says so.

**Why.** "The run that opens a session" is the first one a runner claims, and
only the hub's history says which that is. A session whose first run was
refused, cancelled on its claim or withdrawn exists on no runner; `yad hub`
sent its next run with `new: false`, the runner refused it as a session it
does not hold, and every later run in the session went the same way. Deciding
at offer time needs the runner to agree on what "bound" means: the hub binds
at the answer acknowledging a claim, so the runner now records that answer
(`runs.acknowledged`) and a restart keeps exactly the sessions the hub bound.
Before, a restart withdrew every claim that had not begun to prepare — an
acknowledged one waiting for its `start_at` included — and deleted its
session, which the hub then continued.

**Decided without asking.** A run that names no sources — as a continuing
run usually does — is sent with the sources its session's first run named,
whether it goes out new or continuing. The first run's and no other's: a later
run naming different sources is refused by the runner, and must not change
what the rest are sent. A runner builds a
session's workdir from the first run of it that prepares: a run opening a
session late would otherwise build it empty, and so would a continuing run
after the opener was reported lost at a restart before it prepared (review
found that second case). A runner takes a continuing run naming its session's
own sources as naming none. HUB.md §8 asks the same of every hub.

**What neither side can see.** An acknowledgement the hub sent and the runner
never read before it stopped: the hub bound the session, the runner withdrew
it, and the next run, sent as continuing, is refused as a session the runner
does not hold. It needs a crash inside one round trip, and the refusal names
the cause; closing it would need the runner to accept `new: true` for a session
it holds that never ran a turn, which the owner did not choose. A failed
write of the acknowledgement on the runner ends the same way after a restart.
Holding such a claim back to retry the write was tried in review and is worse:
everything else on the runner reads a pending claim as one the hub never
acknowledged, so a cancel, a drain or a stop would withdraw a run the hub
holds, and a write that kept failing would resync the hub with no backoff.

**Considered.** Keeping `new` fixed at submit and failing the rest of a session
whose first run ended unbound, telling the submitter to start again — every
refused first run would cost its whole session. Having the runner accept
`new: true` for a session it holds that never ran a turn — a runner-side guess
where the hub has the facts.
