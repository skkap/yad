---
date: 2026-09-23
---

# Six hub behaviours settled by Zumino's hub

Zumino built a hub from HUB.md and the OpenAPI document alone (DEV-120), the
second clean-room reading after DEV-110's
([0047](0047-four-v1-protocol-rules-settled-by-the-clean-room-check.md)), and
listed six behaviours the page left open. Two of them showed `yad hub` doing
something it should not. They are recorded together because they are one
kind of thing: a rule a hub has to know and could not read anywhere.

## No offer in a session whose close is out

**Decision.** A hub offers nothing in a session it has sent `close_session`
for and not yet heard closed. The session's queued runs wait and end with it
when the close is reported. HUB.md §8.

**Why.** A runner acts on the controls in an answer before the offers in it,
so a run offered beside a `close_session` for its session meets a session
already closing and is refused as `session_closed`. The close meant the run
to end `cancelled`; it ended `failed`, blamed on the runner. `yad hub` did
exactly this — `OfferCandidates` looked at whether a session was bound and
busy, never at whether its close was asked for — whenever a run sat queued
in a session with nothing held, say for want of capacity, when the close was
asked for.

**Considered.** Letting the offer go and reading the refusal as the close —
a hub would have to tell this `session_closed` from every other one, and the
run's record would still say failed.

## A run listed twice in one sync

**Decision.** A runner lists each run at most once in a sync. A hub handed
two listings of one run may apply either; `yad hub` applies the last. HUB.md
§3.

**Why.** A yad runner never sends a duplicate — it builds the list through a
set — so the question is only what a hub owes a runner that does, and nothing
downstream depends on the answer. Refusing the sync was the one choice ruled
out in spirit: 0047 lets a hub refuse over `run_id`, but a refused sync
renews no lease, and every run the runner holds would pay for one bad line.
It was left permitted rather than required only because nothing a runner
sends will ever meet it.

## A capability document naming another runner

**Decision.** A sync whose capability document's `runner_id` is not the
path's is refused `400 invalid`, as one whose body's is already was. HUB.md
§3, and conformance checks it (`sync/document-runner-id-matches-the-path`).

**Why.** The document is stored as the syncing runner's and decides what it
is offered and which controls it is sent. `yad hub` stored it without
looking at its id, so a runner could be described by another's harnesses and
features. Refusing costs a real runner nothing: a yad runner's document and
its path both carry the one id in its profile.

**Considered.** Ignoring the document's id and storing the rest — the hub
would hold a document that contradicts itself, and the next reader of it
would have to know which half to believe.

## A close from a runner not advertising `close_session`

**Decision.** Believed. A feature gates what a hub sends, never what it
accepts. HUB.md §7 and §8.

**Why.** The feature says the runner acts on the control. A close is a fact
reported by the machine whose disk the workdir is on, and dropping it would
leave the session's queued runs to be offered and refused. HUB.md §7's table
had said the feature gated `closed_sessions` too, which is what made the
question look open.

## A result for a run the hub ended before any claim

**Decision.** `409 conflict`, the hub's state standing — the rule for any
result differing from a terminal state the hub holds, stated now for a run
that ended before a runner claimed it: a refusal arriving for a run
cancelled while offered. `403 not_holder` is as final to a runner and also
conforming. HUB.md §6.

**Why.** Both `yad hub` and Zumino's hub already answered `409`, and the
runner treats the two codes alike — the report is done. Only the page was
silent.

## Abandon-after and the hub's own downtime

**Decision.** Counting silence from the last sync the hub answered, and
making abandon-after longer than the lease, stay musts. Not giving a runner
up for silence the hub caused is a should, done however suits the hub.
HUB.md §5.

**Why.** HUB.md described `yad hub`'s guard — it abandons nobody until it has
itself been up for a whole abandon-after — in a sentence that read as a rule.
That guard fits one long-lived process and never fires on a hub redeployed
more often than its abandon-after, which is Zumino's case. A runner cannot
see which a hub does, so no runner depends on it; what it protects is the
hub's own users, from a long outage closing their sessions.

**Considered.** Making the guard a must in `yad hub`'s form — wrong for every
hub that restarts on deploy. Making "discount recorded downtime" the must — a
bookkeeping rule no runner can observe and no suite can check.
