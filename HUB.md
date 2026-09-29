# Building a hub for the v1 protocol

A **runner** is a `yad` process on someone's machine that runs coding-agent
harnesses — Claude Code, Codex — for whoever gives it work. A **hub** is
whatever gives it work. This page is everything a hub has to do to be one,
written for someone who has never opened this repository.

It is generic on purpose. How your product queues work, authenticates people
or stores runs is your own business; what follows is only the part a runner
can tell the difference about.

**This page and [`protocol/v1/openapi.yaml`](protocol/v1/openapi.yaml) are the
contract.** The document has the shapes and a description of every field;
this page has the rules — the behaviour behind the calls, which no schema can
state. You should need nothing else. If you find yourself reading Go to answer
a question about behaviour, that is a gap here: report it.
[`ARCHITECTURE.md §2`](ARCHITECTURE.md#2-the-protocol--v1) says *why* the
protocol is shaped as it is, and [`DOMAIN.md`](DOMAIN.md) defines its words;
neither is a prerequisite. Every other link from here into the repository —
a decision record, a Go file — is there for the reasoning behind a rule this
page states in full, so a copy of this page and the document handed over
without the repository is still the whole contract. Paths are the
repository's: `protocol/v1/openapi.yaml` is the document, wherever you were
given it.

`yad hub`, in [`internal/hub`](internal/hub), is the reference
implementation, and `yad conformance` checks any hub against this page from
outside (§12). Where this page says "`yad hub` does X", X is one conforming
choice among several; where it says *must*, a runner depends on it.

**Contents**

1. [Overview](#1-overview)
2. [Wire basics](#2-wire-basics)
3. [The calls](#3-the-calls)
4. [Runs](#4-runs)
5. [Leases, timings, and runners that go away](#5-leases-timings-and-runners-that-go-away)
6. [Events and results](#6-events-and-results)
7. [Controls and features](#7-controls-and-features)
8. [Sessions](#8-sessions)
9. [Grants](#9-grants)
10. [Errors and `next_action`](#10-errors-and-next_action)
11. [Versioning](#11-versioning)
12. [Proving it: `yad conformance`](#12-proving-it-yad-conformance)
13. [Checklist](#13-checklist)

## 1. Overview

**Runners call out; a hub never calls in.** A runner sits behind NAT and
listens on no port, so everything travels as the runner's own HTTPS request
and your answer to it. There are five calls:

| call | when | what it does |
|---|---|---|
| `POST /runners/register` | once, from `yad connect` | a registration token in, a runner credential out |
| `POST /runners/{runner}/sync` | every 5–60 s, at the interval you name — 3 s while you hold work for it, if you choose | the runner's health and the runs it holds in; new runs and instructions out |
| `POST /runs/{run}/events` | about every second while a run produces output | a batch of what happened in a run |
| `POST /runs/{run}/result` | once per run, retried until you answer 2xx | how the run ended |
| `POST /runners/{runner}/deregister` | when the runner is leaving for good | its credential dies and you settle what it held |

**The sync is the whole conversation.** One call is the heartbeat, the claim
of what you offered last time, the lease renewal on every run the runner
holds, its health report and its ask for work. Your answer carries the runs
you offer, the controls you want acted on, and when to sync next. There is no
push: a cancel you decide on now reaches the runner in its next sync.

**A run's life, from the hub's side:**

```
            you queue it
                 │
              queued ◄──────── the next sync did not list it,
                 │                 or the offer's lease lapsed
                 │ you offer it in a sync response         ▲
              offered ─────────────────────────────────────┘
                 │ the runner's next sync lists it
              claimed ─► preparing ─► running ◄─► waiting
                 │           │           │          │
                 └───────────┴───────────┴──────────┴──► succeeded | failed |
                                                         cancelled | timed_out
                   (a result, from the runner)
            any held state ── lease lapses ──► lost   (your decision;
                                                      a claim you cancelled: cancelled, §3)
```

`queued` and `offered` are yours; the protocol never carries them. From
`claimed` on, the runner reports the state in every sync until the run ends,
and the terminal state arrives in the result. `lost` is the one terminal
state you decide rather than hear — and `cancelled`, for a claim you cancelled
that the runner withdrew unstarted (§3). §4 has the full state machine.

**A session is a conversation that stays on one machine.** Every run belongs
to one session. The first run in a session binds it to the runner that claims
it, because the harness's transcript and the working directory are on that
runner's disk; every later run in the session goes to that runner, one at a
time (§8).

**What is yours to decide**: how runs are queued and prioritised, which runner
you prefer among several that could take a run, what `lost` means to your
users, who may submit work, and how long you keep anything. **What is not**:
anything about how the harness runs on the machine — permission mode,
sandbox, which account it spends. Those are the runner owner's, and no field
in the protocol can set them.

## 2. Wire basics

**Start from the document.** `protocol/v1/openapi.yaml` is generated from the
Go types in [`protocol/v1`](protocol/v1) and committed; a test fails when the
two drift. Generate your types from it:

```
npx openapi-typescript protocol/v1/openapi.yaml -o src/protocol.ts
```

Every body a runner sends and expects is in there, with a description on each
field, and so are the two headers you check: `Yad-Protocol` is a required
header parameter on each operation, and the bearer is the `runner` security
scheme.

**The base URL is yours.** Every path in the document is relative to the
connection URL a runner is given, so a hub may mount the protocol anywhere:
`https://example.com/yad/v1` is a fine connection URL, and the runner then
calls `https://example.com/yad/v1/runners/{runner}/sync`. The document's
`servers` entry says `/v1` only because that is where `yad hub` mounts it.

**Every request carries:**

| header | value |
|---|---|
| `Authorization` | `Bearer <registration token>` on register, `Bearer <runner credential>` on every other call |
| `Yad-Protocol` | `1` — the protocol's major version |
| `Content-Type` | `application/json` |
| `User-Agent` | `yad/<version>`, for your logs; the version a hub acts on is `capabilities.yad_version` |

**Two wire rules the types do express, and which hand-written encoders
override anyway:**

- **An absent list is an empty list.** A list the document marks optional is
  omitted when empty, not sent as `[]` or `null`. Your encoder must do the
  same, or a runner that round-trips your response will disagree with you
  about what you offered. A list the document marks **required** is always
  sent: of the lists a runner sends, that is a batch's `events` and a
  capability document's `harnesses`. `yad hub` refuses a batch with no
  `events` field as `invalid`.
- **Unknown fields are ignored.** Every object allows additional properties,
  so a field added within v1 never breaks an older hub or runner. Do not
  reject what you do not recognise.
- **Unknown values are not.** Every enum in the document is closed for all
  of v1 ([0047](docs/decisions/0047-four-v1-protocol-rules-settled-by-the-clean-room-check.md))
  — an event's `kind`, a held run's `state`, a result's `state`, a closed
  session's `reason`, a control's `kind`, and the rest the document declares.
  You may refuse a body carrying a value outside one: `yad hub` does, with
  `400 invalid`, as it refuses any body that fails the document's schema. No
  runner sends you one unless you asked for it. A value added to one of these
  sets arrives only behind a feature — sent to a hub only once it has
  advertised the feature in `hub_features`, as a new control goes to a runner
  only once it has advertised one in `protocol_features` (§11). Without that
  rule a runner sending a new event kind to an older hub would meet `400
  invalid` and drop the whole batch (§6), events of the kinds you know
  included.

**Types.** Times are RFC 3339 strings (`format: date-time`); a runner sends
UTC. Every duration is an integer number of milliseconds and says so in its
name (`lease_ms`, `max_wait_ms`). Ids — runner, run, session — are opaque
strings. A yad runner's id is 16 hex digits; `yad hub` accepts any 1–128
letters, digits, dots, dashes or underscores, since the id becomes a path
segment.

**Answer within 30 seconds.** A runner gives up on a request after 30 s and
treats it as a failure to retry. A sync is small; there is no reason for one
to take that long.

**Every error is the envelope**, with a `next_action` — §10:

```json
{"error": {"code": "invalid", "message": "...", "next_action": "..."}}
```

## 3. The calls

Each call below says who authenticates it, what the request means, what you
must do, what you answer, and how you refuse. The refusals name the status
and code `yad hub` uses; §10 says which of them the protocol fixes and which
are yours to choose.

Before anything else, on **every** call: **refuse a missing or wrong
`Yad-Protocol` header with `426 unsupported_protocol`, before reading the
body.** A body shaped for another version fails validation in ways that say
nothing about the cause, and on `register`, reading the body first could burn
the operator's token on a request you were always going to refuse.

**When a request is wrong in more than one way**, that header check is the only
precedence the protocol fixes. The rest is `yad hub`'s order, one conforming
choice, and the one a runner has been tested against:

1. a `POST` whose `Yad-Protocol` is missing or wrong: `426
   unsupported_protocol` — before the route, so an unknown path says so too;
2. a path that is no operation: `404 not_found`; a method an operation does
   not have: `405 invalid`;
3. the body: `413` when it is too large (16 MiB on events and result, 1 MiB
   elsewhere), and `400 invalid` when it is not JSON or fails the document's
   schema — a missing required field, a value outside an enum. This comes
   *before* the credential: `yad hub` reads and validates the body first;
4. the credential: `401 unauthorized`, or `403 unauthorized` for another
   runner's on a call that names a runner in its path. On register this step
   is only that a bearer is there;
5. the call's own rules — a sync's `runner_id` against the path, then its
   version floor; a batch's size, then its `seq`s; a register's runner id and
   name, then its version floor, then the token itself, so a runner refused
   for its version keeps its token (§11);
6. the run the path names: `404 not_found` for one you never heard of, then
   `403 not_holder` for one the caller may not report on, then `409 conflict`
   for a result differing from the state you hold.

Where two orders would give different answers, a runner acts on the code
(§6): an event batch with `seq: 0` for a run the caller does not hold is `400
invalid` from `yad hub` and `403 not_holder` from a hub that checks the holder
first, and the runner drops the batch on either.

### `POST /runners/register`

**Authenticated by a registration token your hub issued**, as
`Authorization: Bearer <token>`. How you issue tokens is yours — `yad hub`
has `yad hub token create`, which prints one that expires in an hour. The
runner's owner passes it to `yad connect <your connection URL> --token -`.

**Request:** `{"capabilities": {...}}`, the runner's **capability document** —
its id, name, version, OS, arch, labels, every harness in its catalog, host
tools, capacity and protocol features. Store it; it is what tells you which
runs this runner can take (§4) and which controls it acts on (§7), until a
sync carries a newer one.

**What you must do:**

- **Register no runner without a token, and none with a bearer you did not
  issue.**
- **A registration token registers one runner once.** Presented a second
  time, for any runner, it is refused — it is spent whether or not the runner
  it registered still exists. That is the whole of the rule: a token that
  *registered* a runner is spent. A registration you *refused* — a body that
  does not validate, a runner id that conflicts — need not spend it, and the
  protocol does not say either way. `yad hub` does not, so an operator who
  fixes the request can retry with the same token.
- **Issue a runner credential** and answer it in `runner_credential`. It is
  the only secret the runner keeps, and it authenticates every later call.
  Keep only what recognises it — `yad hub` stores a SHA-256 hash.

- **Re-register a runner id you already know only with a token issued for
  that runner.** Runner ids are not secret, so a token that could re-register
  *any* known runner would hand that runner's sessions, and the grants
  delivered into them, to whoever held a token. `yad hub` refuses a known
  runner id with `409 conflict` unless the token was issued for that runner
  (`yad hub token create --runner <id>`); such a token replaces the runner's
  credential, the old one stops working at once, and the runner's sessions
  stay bound to it. How you tie a token to a runner is yours; that you do is
  not.

**Response:**

| field | rule |
|---|---|
| `runner_credential` | required |
| `sync_interval_ms` | required; 5000–60000 inclusive (§5) |
| `lease_ms` | required; never shorter than `sync_interval_ms` (§5) |
| `hub_features` | optional; v1 defines none, so omit it |
| `min_version` | optional; the oldest yad you accept (§11) |

**Omitting both optional fields is a complete, conforming hub** — nothing
checks either, and a hub that never refuses on version never emits
`version_too_old`.

**Refusals** (`yad hub`): `400 invalid` for a body that does not validate or a
malformed runner id; `401 unauthorized` for no bearer, a token never issued,
already used, expired, or issued for another runner id; `409 conflict` as
above; `426 version_too_old` when the runner is under your `min_version` —
checked *before* the token is spent, so the upgraded runner can still use it.

### `POST /runners/{runner}/sync`

**Authenticated by the runner credential.** The credential must belong to
the runner the path names, and the body's `runner_id` must equal the path's.

**Request** — every field is described in the document; the ones you act on:

| field | what it tells you |
|---|---|
| `fingerprint` | an opaque hash of the runner's capability document. Compare it with the one that arrived with the document you hold |
| `capabilities` | present on the first sync of every runner process, after the fingerprint moves, and after you asked with `report_capabilities`. When present it replaces what you hold |
| `health.free_capacity` | how many runs you may offer in this response: `total` overall, and `by_harness` per harness (§4) |
| `health.harnesses` | per harness, `ready` and each account's state. A harness with `ready: false` has every account at a usage limit or needing a login; the runner will not claim for it. A harness missing from the list, or the list missing, says nothing either way: `yad hub` does not read `ready` at all, and offers by the capability document and the free capacity alone |
| `health.draining` | the runner has stopped taking work (§7) |
| `runs` | **every run the runner holds**, with its state. Listing is claiming and lease renewal (below) |
| `closed_sessions` | sessions the runner has closed, repeated until answered (§8) |
| `logins` | the hub logins you started on this runner, each repeated until one carrying its end is answered (§7) |

The rest of `health` — load, disk, spool and outbox depth, recent errors — is
for your operators and dashboards. It is the runner's own words, bounded, and
never carries a credential. All of it is optional: `load`, `disk_free_bytes`,
`spool_depth` and `outbox_depth` are left out when they are 0, and a runner may
leave them out for any other reason. Read an absent one as 0, or as not said.

**A sync may be refused only over what routing reads**
([0047](docs/decisions/0047-four-v1-protocol-rules-settled-by-the-clean-room-check.md)):
`runner_id`, `fingerprint`, `health.free_capacity.total`, and each listed run's
`run_id` and `state` — and, inside a part the runner chose to send, the fields
the document marks required there: a capability document's, a closed
session's, a harness's health. Never over a dashboard field, whether it is
missing or holds a value you did not expect. A refused sync renews nothing, so
a hub strict about a dashboard field loses every run a runner holds as the
leases lapse. `yad hub` validates the body against the document, which marks
exactly these required, and refuses a sync missing one with `400 invalid`.
Conformance sends a sync whose health is its free capacity alone, and wants it
taken.

**What you must do, in this order** (the order is `yad hub`'s, and each step
says why it goes where it does):

1. **Settle what time has decided** (§5): offers whose lease lapsed go back
   in the queue, held runs whose lease lapsed are lost — a claim you have
   asked to cancel is cancelled instead (below) — and runners silent past
   your abandon-after lose their sessions — so a runner back from a long
   absence hears what became of its runs rather than renewing them.
2. **Take the document** if the request carries one: it replaces the one you
   hold. A document whose `runner_id` is not the path's is refused with the
   whole sync (Refusals, below) — stored, it would describe this runner by
   what another advertised. If it carries none and the fingerprint differs from the one you hold,
   answer with a `report_capabilities` control, and until the document
   arrives, send no control gated on a feature and offer no run that needs one
   (§7): what the runner acts on is unknown. Register carries a document and
   no fingerprint, so after register you hold a document with none: `yad hub`
   takes the first sync's fingerprint as differing, and a first sync that
   carries no document — which a yad runner never sends — is answered with
   `report_capabilities`.
3. **Refuse a runner under your `min_version`** (§11), judged on the newest
   document: the one this request carries, or else the one you hold. A sync
   has no version field of its own, but the first sync of every runner
   process carries its document — so a runner its owner has just upgraded is
   judged on its new version, not the one you refused. A refused sync renews
   nothing, claims nothing and stores nothing.
4. **Settle each listed run:**
   - **offered to this runner and not yet claimed → it is now claimed.**
     Record it as held by this runner, bind its session to this runner if
     this is the session's first claim (§8), and start its lease.
   - **held by this runner → renew its lease**, and record the state, `reason`
     and `resumes_at` it reports.
   - **anything else → answer with a `cancel` control for that run.** A run
     you never offered to this runner, offered to another, already finished or
     already lost, whose offer to this runner lapsed before this claim
     arrived, or never heard of: silence leaves the runner holding something
     you have no record of. Do not offer that same run in the answer carrying
     its cancel — the runner would have to guess which one you meant — nor
     any other run in its session: a runner stops a run it is executing only
     after claiming the offers beside the cancel, and refuses a second live
     run in one session. If it lists as `claimed` a claim you ended
     when its lease lapsed, and that claim bound its session, unbind the
     session (§8).
   - then add the controls you have for each held run (§7).
   - **a claim you have asked to cancel that this sync leaves out → it is
     cancelled.** The runner withdrew it unstarted and owes no result (below).

   **A runner lists each run at most once in a sync**, and a yad runner never
   lists one twice. A hub handed two listings of one run may apply either:
   `yad hub` applies the last, and applying the first is as conforming.
   Nothing depends on which, and conformance sends no such sync
   ([0048](docs/decisions/0048-six-hub-behaviours-settled-by-zuminos-hub.md)).
5. **Take the closes** in `closed_sessions` (§8), and add a `close_session`
   control for each session you are closing on this runner. Take the
   `logins` too, and add the login controls each open one still needs (§7) —
   a draining runner still reports them.
6. **Take back every run you offered this runner that this sync did not
   list.** It was never received. It goes back in your queue now, in this
   sync — not when its lease lapses — for this runner or another, and step 7
   may offer it again in this same answer. Conformance checks this: it leaves
   an offer out of one sync and lists it in the next, which a hub that took
   the offer back answers with a `cancel`.
7. **Offer new runs** — unless the runner is draining or you have asked it to
   drain — within the free capacity it declared, only for harnesses it can
   drive, and only what its features allow (§4). Each offer is held for this
   runner for `lease_ms` (§5).
8. Answer `next_sync_ms` and `lease_ms` (§5), and `min_version` if you have
   one.

**Claim by listing.** A run you offer in a sync response is *not* claimed by
that response. It is claimed when the runner's **next** sync lists it. A hub
that treats the offer as the claim loses every run a runner never got — a
response lost on the network, or an offer the runner declined by leaving it
out. The runner takes capacity for an offer before it syncs again, so it can
always start what it claims, and it syncs again at once when a response
carries offers, so a run starts about one round trip after it is offered.

**A run starts only after its claim is acknowledged.** The runner starts a
claimed run once a sync listing it has been answered *without a `cancel` for
it*. That is what makes the cancel in step 4 safe: a run you cancel before the
runner's listing is answered never starts, and the runner drops it without
reporting anything.

**A claim you cancel that the runner withdraws ends `cancelled`, never `lost`**
([0061](docs/decisions/0061-a-cancelled-claim-the-runner-withdraws-ends-cancelled.md)).
If the answer that acknowledged a claim is lost in transit, you hold the run
as claimed while the runner is still waiting to hear so. A cancel you ask for
then reaches it first, so it withdraws the claim — the run never starts and no
result follows — and its next sync leaves the run out. Record the run
`cancelled` at that sync or, if no sync comes, when its lease lapses: it ended
the way you asked, and `lost` would tell your user that something failed when
nothing did. The rule covers only a run you hold as `claimed` and have asked
to cancel. A claim nobody asked to cancel stays held until its lease lapses,
and is lost; a run a sync has reported `preparing` or later owes a result, and
is lost if none comes. A runner that deregisters holding a cancelled claim is
the same case: end the claim `cancelled` (§5). A withdrawn claim takes the
session it opened with it, so a session that claim bound is no longer bound
(§8).

**Response:** `next_sync_ms` and `lease_ms` (required), `runs` (the offers),
`controls`, `min_version`. Omit `runs` and `controls` when empty.

**Refusals** (`yad hub`): `400 invalid` for a body that does not validate, or a
`runner_id` differing from the path's in the body or in the capability
document it carries
([0048](docs/decisions/0048-six-hub-behaviours-settled-by-zuminos-hub.md)); `401 unauthorized` for no credential or
one you do not know; `403 unauthorized` for a credential belonging to another
runner; `413` for a body of a mebibyte or more; `426 version_too_old` under
your floor. Conformance checks that a mismatched `runner_id` — the body's or
the document's — and another runner's credential are refused, and that a body which is not JSON is refused
as `invalid`. A runner stops syncing your
hub on `unauthorized`, `runner_revoked`, `version_too_old` and
`unsupported_protocol` until its owner acts; on anything else it backs off —
1 s doubling to 30 s — and tries again (§10).

### `POST /runs/{run}/events`

**Authenticated by the runner credential.** The path names the run; the
credential alone says which runner is calling.

**Request:** `{"events": [...]}`, events of this one run in `seq` order. A yad
runner sends a batch about every second, or sooner at 100 events, from a
durable spool, so a network failure loses nothing and a batch may repeat
events you already hold. 100 is the runner's habit, not the protocol's
limit: `yad hub` takes up to 1000 in one batch, for runner builds that batch
differently, and refuses more. A batch whose `seq`s repeat or are out of
order is not refused by `yad hub` — each event is stored once by `(run,
seq)`, the first copy standing, and `acked_through` is worked out after.

**What you must do:** accept events only from the runner the run was claimed
by — before and after it ends — store each one once by `(run, seq)`, and
answer how far the run's stream is now complete. §6 has the rules.

**Response:** `{"acked_through": N}` — the highest `seq` up to which you hold
every event of this run with no gap.

**Refusals** (`yad hub`): `400 invalid` for a `seq` below 1 or more than 1000
events in one batch; `401 unauthorized`; `403 not_holder` for a run this
runner does not hold; `404 not_found` for a run you have never heard of;
`413` for a body of 16 MiB or more — the runner halves the batch and tries
again. A `413` is acted on by its status, whatever code it carries: `yad hub`
sends `invalid` with it, and the runner still halves rather than drops (§6).
Whatever limit you set, refuse a body over it with `413` and never `400
invalid`, which the runner drops. Conformance sends a 17 MiB batch and wants a
2xx or a `413`.

### `POST /runs/{run}/result`

**Authenticated by the runner credential.**

**Request:** the run's terminal report — `state` (one of the five terminal
states), `final_text`, `error` with a class and message, `usage` by model,
`metrics`, and `last_seq`, the seq of the run's last event. The runner writes
it durably before the first attempt and retries until you answer 2xx.

**What you must do:** accept it from the runner the run is offered to *or*
claimed by — an offer only while it is open; apply it at most once;
acknowledge the same state again; refuse a different one with `409 conflict`.
§6 has the rules, including why the door is wider than for events and how
long an offer keeps it open.

**Response:** `{"ok": true}`.

**Refusals** (`yad hub`): `400 invalid` for a state that is not terminal;
`401 unauthorized`; `403 not_holder` for a run the caller does not hold and
is not offered now; `404 not_found`; `409 conflict` when you already hold a
different terminal state; `413` for a body of 16 MiB or more, as for events.
Conformance checks the non-terminal state, the `413`, and the `403 not_holder`
for a refusal sent after the offer was taken back.

### `POST /runners/{runner}/deregister`

**Authenticated by the runner credential**, which must belong to the runner in
the path.

**Request:** `{"reason": "..."}`, optional — the runner's own words for why it
is leaving. Untrusted text: keep it bounded and printable if you store it.
`yad hub` keeps 200 characters of one line.

**What you must do:** retire the credential and settle everything the runner
held — §5 has the rule, because it is the same question as a runner that
stops syncing.

**Response:** `{"ok": true}`.

**Refusals** (`yad hub`): `400 invalid` for a body that does not validate;
`401 unauthorized`; `403 unauthorized` for another runner's credential; `413`
for a body of a mebibyte or more.

A current yad runner does not call `deregister` yet: `yad disconnect`, which
will, is still being designed. Implement it anyway; the conformance suite
lists it among the rules it cannot check (§12).

## 4. Runs

### What a run carries

A run is one turn of one harness against one session: one instruction in,
one terminal state out. It names its `session`, `harness` and `model`
explicitly — a runner infers none of them — and carries a `brief` (the
`instruction`, and optional `context`, the run's standing background, below),
an optional `effort` (how hard the harness thinks, in its own terms,
as `model` is), optional `sources` to build the session's working directory
from, optional `grants` (§9), and optional timings: `start_at`, `max_wait_ms`,
`wall_clock_ms` and `inactivity_ms`. A run opening a session may also name,
in `session.fork_from`, a session whose conversation the new one starts from
(§8). The document describes each field and its
absent case.

There is deliberately no permission, sandbox or tool-policy field. A hub
cannot set or widen what a harness may do on someone's machine.

### Context and instruction

**`brief.context` is the standing background of the run that carries it —
every run, the ones continuing a session included.** It reaches the harness
outside the conversation, where it outlasts a compaction:

- **Claude Code**: appended to the system prompt, rendered afresh on every
  request. Claude would otherwise record a conversation's system prompt on its
  first request and replay that record on every resume, so a continuing run's
  context never reached the model — only the session's first run's did. The
  runner turns the record off.
- **Codex**: the thread's developer instructions. Codex reads new ones on a
  resumed thread only once a compaction rebuilds the thread's opening, so the
  runner also puts the run's context in the thread as a developer message just
  before the run's turn.

So a continuing run's context is the latest background the model has read,
whatever the session's earlier runs carried. What a context does **not** do is
erase an earlier one: Claude's system prompt holds this run's context alone,
but a Codex thread keeps each earlier run's context in its history, and both
harnesses keep every earlier answer. A run without a context runs with none of
its own, not with the session's last.

**What goes where.** Put in `context` what every run of the session must
obey or know, and send it **whole on every run** — standing rules ("this is a
smoke test: change nothing"), who the agent is working for, the task it
belongs to, a codeword. It is not the turn's request and never appears as one.
Put in `instruction` what this run is to do — the new message, the question,
the task's next step. It is the run's one user turn, and becomes part of the
conversation every later run reads. A rule sent only in the first run's
context, or only in an instruction, is history to later runs rather than a
rule, and a harness may weigh it that way.

### Rules the schema cannot state

**A `Source` is exactly one of `git` or `path` — and your generated types will
not enforce it.** The document says the rule with a `oneOf`, but
`openapi-typescript` renders `oneOf` as a plain union rather than an exclusive
one; its own documentation notes this "mimics behavior closer to `anyOf`". So
the generated `Source` will happily accept **both** fields set. It rejects an
object with **neither key present** — no arm of the union accepts that — but
not `{"path": ""}` or `{"git": {"url": ""}}`, which satisfy the types and are
"neither" once here: an empty string is the same as unset. An empty form field
reaches you as exactly that shape. The constraint is in the document for you
and for tooling that reads it, not as a type you can lean on. **Check it
yourself.** A run naming both, or neither, must be refused.

**Every run you offer must satisfy rules the schema cannot state**, and
conformance validates *every run your hub offers it* — it never sends you an
invalid run to see whether you refuse it, so this is a rule about your output
and not your input. Here is all of it:

- `run_id`, `session.id`, `harness`, `model` and `brief.instruction` are each
  non-empty.
- `session.mode`, when given, is `per_run` or `live`.
- `session.fork_from`, when given, rides on a run with `session.new: true` and
  names another session than `session.id`.
- `effort`, when given, is at most 64 bytes of letters, digits, `-` and `_`.
  Which levels exist is the harness's business; the shape is the runner's,
  because a harness's refusal quotes the word, and a long one hides it (§7).
- each `Source` is exactly one of `git` or `path`, and a `git` source has a
  non-empty `url`.
- each grant is delivered `as` either `env` or `file`.
- each grant name matches `[A-Za-z_][A-Za-z0-9_]*` — it is an environment
  variable name, and also the filename a file grant is written to.
- no grant may be named `PATH` or `HOME`, or begin `LD_` or `DYLD_`: those
  would redirect the harness or its loader.
- no grant may name a variable that chooses whose credential a harness uses or
  which home it logs in from, because the run would then spend a credential its
  account knows nothing about while its events still named the account
  ([0040](docs/decisions/0040-a-grant-may-not-move-a-run-off-its-account.md)):
  `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`,
  `ANTHROPIC_PROFILE`, `ANTHROPIC_FEDERATION_RULE_ID`,
  `ANTHROPIC_ORGANIZATION_ID`, `ANTHROPIC_CONFIG_DIR`, `CLAUDE_CONFIG_DIR`,
  `CLAUDE_SECURESTORAGE_CONFIG_DIR`,
  `ANTHROPIC_BASE_URL`, `ANTHROPIC_CUSTOM_HEADERS`, anything beginning
  `CLAUDE_CODE_USE_`, `CODEX_HOME`, `OPENAI_API_KEY`, `CODEX_API_KEY`,
  `CODEX_ACCESS_TOKEN`, `OPENAI_BASE_URL`,
  `CODEX_REFRESH_TOKEN_URL_OVERRIDE` and `AWS_BEARER_TOKEN_BEDROCK`, whatever the run's harness. If the project itself needs
  one of these keys — to run its tests, say — send it under another name and
  have the brief say which. This list is complete. `protocol/v1/grant.go`
  holds the same one, and a test fails when the two, or the document's
  description of `Grant.name`, differ.
- every name above is matched whatever its case.
- no two grants share a name, and no two **file** grants have names differing
  only by case — on a case-folding filesystem they become one file holding
  silently the second value.

A run breaking any of them is refused **whole**, not stripped of the offending
part.

**A run id is used once.** A runner never runs the same run id twice: offered
a run id it already ran — say, one it reported `lost` after a restart — it
refuses it. To try work again, make a new run.

A claim you cancelled before the runner's listing of it was answered is not a
run it ran. The run never started, and the runner removes it, and the session
the claim opened unless its owner asked to close that session meanwhile (§8)
— so the same run id
may be offered again, to it or to another runner. `yad hub` does exactly that
with a run whose offer lapsed and whose late claim it cancelled (§5).

### Who may be offered what

**Only a harness the runner can drive.** In the capability document, a
harness can take runs when its `kind` is `first-class`, `present` is `true`
and it has no `error`. `present` means the runner found a binary it would
start — not that the binary works; a present harness with an `error` takes no
runs, and an `error` with `present: false` means an override that names
nothing and nothing on `PATH` either. A `recognised` harness is detected and
reported so the gap is visible, and is never the target of a run. `warnings`
never make a harness unusable: they are for whoever owns the machine. A
runner refuses a run for a harness it cannot drive, but the run has held your
queue until then.

**Host tools** — `git`, `gh`, `docker` — are reported the same way, if you
route on them: a tool is usable when it is `present` with no `error`, and
`logged_in` too for a tool that has a login. `login_hosts` says which hosts a
`gh` is signed in to, so you can tell a runner that can reach your
repositories from one that cannot.

**A harness's `models` are what to offer, not a gate.** The runner asks the
harness itself, without spending a token, which models each login a run may
use is offered — Claude's `list_models`, Codex's `model/list` — and reports
them in the harness's order, with each account's own list under the account
when the harness listed one for it: two accounts on different plans can be
offered different models. `models_source: catalog` is a harness that could
not be asked, and a fixed list from the runner in its place; why it could not
is not sent — the machine's owner sees it in `yad doctor`. Neither is a
limit: a run may name any model, and the harness decides whether it exists.

**Prefer a harness whose health says `ready`.** An offer for a harness whose
every account is at a usage limit or needs a login is not refused — there is
nothing wrong with the run — but it is left unlisted, and comes back to your
queue a sync later.

**Never offer more than the free capacity, on either count.** The sync
request's `health.free_capacity` is **already net of the runs the runner
holds**, on both counts — `total` after everything it holds, and each
`by_harness` figure after what it holds of that harness. It is what is left,
reserved for new offers. Offer up to it as sent; do not subtract the runs
listed in the same request, or you count them twice and offer too little.

`total` bounds the whole response — and `by_harness` bounds each harness
**independently of it**: a sync declaring `total: 4` with `by_harness: {claude:
1}` will take one Claude run, not four. A harness **absent** from `by_harness`
has no per-harness bound and is limited by `total` alone; the map carries only
the caps its owner set, and an empty map is omitted entirely. That per-harness
figure is the owner's cap on their own machine. Offer past either and the
surplus is left unclaimed, having occupied your queue in the meantime.

A runner connected to several hubs shares one pool between them and deals it
round the hubs a unit at a time, so what one hub is offered does not depend
on how deep another's queue is. That is the runner's business: you see only
the free capacity it gives you.

**Only what the runner's features allow.** A run carrying a `start_at` still
in the future goes only to a runner advertising `start_at`; a run carrying an
`effort` only to one advertising `effort`; a run whose `session.mode` is
`live` only to one advertising `live_sessions` (none does yet). §7 has the
table. An `effort` and a `fork_from` are the run's harness's to take: when the
runner advertises `harness_features`, ask the list in that harness's report,
not the runner-wide string (§7). Unlike a `start_at`, an `effort` never lapses: on a fleet that
advertises none the run stays queued, and a run continuing a session bound to
a runner that does not advertise it (§8) is never offered at all — tell
whoever submitted it, or submit it without the effort.

**A fork only to the runner holding what it forks.** A run opening a session
with `session.fork_from` goes only to the runner the forked session is bound
to — the transcript it copies is there and nowhere else — and only while that
runner advertises `fork` for the run's harness (§7). Like an `effort`, it never lapses, and it can go
nowhere else: hold it until that runner advertises the feature, and end it
when that runner goes (§5, §8).

**Only sources the runner takes.** A runner whose owner has switched sources
on the machine off says so in its capability document, `path_sources: false`;
absent, it takes them, and no runner sends `true`. Such a runner fails a run
with a `path` source, or a `git` source whose `url` is an absolute path or a
`file://` URL, with `source_refused`. So offer a run opening a session
(`session.new`) with one of those to another runner: this one would refuse it,
and one that takes it may be connected. A run in a session already bound to
the runner can go nowhere else — offer it, and the refusal's message tells
whoever submitted it which setting refused it (decision
[0062](docs/decisions/0062-an-owner-may-switch-sources-on-the-machine-off.md)).

**Session rules** decide the rest (§8): a run in a session bound to another
runner is not offerable here, and neither is a run whose session already has
a run offered or held.

### Run states

```
                 queued ◄──── offer not listed back, or its lease lapsed
                   │                                  ▲
                   ▼  offered in a sync response      │
                 offered ──────────────────────────────┘
                   │  listed in the runner's next sync
                   ▼
claimed ─► preparing ─► running ─► succeeded | failed | cancelled | timed_out
                          │  ▲
                          ▼  │ (limit resets / account frees)
                        waiting ──────────────────────────► timed_out (max_wait)
   any non-terminal state ── lease lapses ──► lost      (decided by the hub;
                                                         a claim it cancelled: cancelled, §3)
```

| state | whose | meaning |
|---|---|---|
| `queued` | hub | waiting for a runner that can take it |
| `offered` | hub | sent in a sync response; not yet listed back. It goes back to `queued` when the next sync leaves it out, or when `lease_ms` passes with no sync claiming it |
| `claimed` | runner | listed in a sync; not yet started |
| `preparing` | runner | building the workdir, running the repository's setup hook, choosing an account |
| `running` | runner | the harness is running. A finished run whose result you have not yet acknowledged stays listed as `running`, so its lease outlasts an outage on your side |
| `waiting` | runner | parked on a usage limit with no process; `resumes_at` says when it expects to continue. It survives a runner restart |
| `succeeded`, `failed`, `cancelled`, `timed_out` | runner | terminal, reported in the result. You record `cancelled` yourself for a run you cancel before any claim, and for a claim you cancelled that the runner withdrew (§3) |
| `lost` | hub, usually | terminal. You record it when a lease lapses or the runner deregisters — except on a claim you cancelled, which is `cancelled` (§3). A runner also *reports* `lost` for a run a previous process of it was holding when it died |

A run reaches exactly one terminal state and never leaves it. Non-terminal
states travel in syncs; the terminal one travels in the result. A run also
reaches `waiting` from `preparing`, when no account was free before the turn
started, and comes back through `preparing` either way.

### Why a run failed: error classes

A failed, cancelled or timed-out run's result carries `error.class`, for a
program to act on, and `error.message`, for a person. The classes are not a
closed set — a class you do not know is a failure to show, not to parse — but
these are the ones worth acting on:

| class | state | what it means for you |
|---|---|---|
| `refused` | failed | the runner would not take the run — invalid, a harness it cannot drive, a session rule broken (a fork of a session it cannot fork among them), an `effort` for a harness whose effort it cannot set. Retrying the same run changes nothing; the message says why |
| `session_closed` | failed | the run names a session the runner has closed or is closing. Start a new session |
| `resume_rejected` | failed | the harness had no conversation to continue: the transcript is gone. Start a new session |
| `session_mismatch` | failed | the harness ran under another session id; the conversation's context is lost |
| `grants_lost` | lost | a run parked on a usage limit was picked up by a later runner process, and its grants, or the credential in a source's URL, did not survive. Submit a new run, with them |
| `max_wait_exceeded` | timed_out | it waited longer than `max_wait_ms` for a free account |
| `wall_clock_timeout`, `inactivity_timeout` | timed_out | stopped by the run's own caps |
| `source_refused` | failed | a source breaks the owner's rules — a path outside the allowed directories, any source on the machine at a runner whose document says `path_sources: false`, a transport the runner does not use |
| `source_failed`, `setup_failed`, `prepare_failed` | failed | the workdir could not be built, or the repository's setup hook failed |
| `prompt_too_long` | failed | the conversation no longer fits the model's context |
| `runner_stopping` | cancelled | the runner cancelled it on its way down, not you |
| `runner_restarted` | lost | the runner process holding it stopped without finishing it |
| `harness_error`, `harness_exited`, `harness_start_failed`, `adapter_error` | failed | the harness failed, or could not be started — including a harness refusing the run's `effort`, in its own words |

`steer_failed` and `interrupt_failed` appear only as `error` *events*, never
in a result: the control did not reach the harness, and the run carried on.

## 5. Leases, timings, and runners that go away

**The interval you name must be between 5 s and 60 s.** `sync_interval_ms` at
register and `next_sync_ms` in every sync response are both bounded,
inclusive — except that `next_sync_ms` may go as low as 3 s, for the answer
below. A hub naming 2 s or 5 min is refused by conformance and clamped by a
runner. `yad hub` uses 15 s. The runner adds ±10 % jitter, syncs at once
when a response carried offers, and backs off from 1 s to 30 s after a failed
sync.

**You may ask a runner back sooner while you hold work for it.** Between
5 s and 3 s is for one thing: a queued run this runner would be offered once
a run it holds ends — its capacity is full, or the run is the next turn in a
session it is running. Without it that run starts up to a whole interval after
the run ahead of it ends; with it, within about 3 s. It is optional, and it is
no help to a runner holding nothing — the run submitted a moment after an idle
runner's sync still waits one interval, since the answer that would have to
change was sent before the run existed. `yad hub` answers 3 s when **all** of
these hold, and its configured interval otherwise
([0063](docs/decisions/0063-a-hub-holding-work-for-a-runner-asks-it-back-in-3-s.md)):

- a queued run is one it would be offered but for its capacity or the live run
  of its own session: a harness it drives and has not capped at zero, in a
  session unbound or bound to it, not closing, and past everything else an
  offer checks — a start moment it cannot hold, an effort it does not take,
  a source on the machine its owner has switched off;
- a run the runner lists as executing — anything but `waiting` — would let
  that queued run go by ending. A waiting run gives its capacity, and its
  session, back at an account's reset, hours off; a runner listing nothing has
  its capacity taken by nothing you can see end — another hub's runs, or none
  at all — so asking either back sooner finds it no freer. When the queued
  run's harness has its own cap full (`free_capacity.by_harness` at 0), only a
  run of that harness ending frees it; otherwise any run ending does;
- it is not draining, and has not been asked to.

An idle fleet with nothing queued, and a run no runner here can take, keep the
normal interval. A runner at capacity for the length of a long run is asked
back five times as often for that long — the price of the next run starting
seconds after it ends.

**Leases.** Every sync renews the lease on every run it lists. A run whose
lease lapses is **lost**: the runner is gone or has stopped talking, and the
run will not be reported. The one exception is a claim you have asked to
cancel, which is **cancelled**: the runner may have withdrawn it unstarted,
which is the end you asked for (§3). Decide what lost means for you, but
decide it; a run in a non-terminal state that nothing will ever end is a queue
that only grows.
Lapsed leases must be found even when nothing syncs — `yad hub` sweeps on a
timer at the sync interval, as well as at the start of every sync — because a
hub whose only runner went away gets no syncs to notice it by.

**A lease runs from when you handle the sync that named it** — on your clock,
never the runner's. `yad hub` reads its clock once, as it starts handling a
sync, and every run that answer claims, renews or offers is leased from that
moment. Measured that way a lease exactly one interval long lapses on a runner
whose next sync is a moment late, which is why the default below is four
intervals and not one.

**Never name a `lease_ms` shorter than the interval beside it — in *either*
response.** Register answers with `lease_ms` next to `sync_interval_ms`, and
every sync answers with `lease_ms` next to `next_sync_ms`; both pairs are
checked, and a hub that gets the sync right and the registration wrong fails.
Getting it wrong punishes the runners behaving best: a lease shorter than the
interval lapses on a runner that synced *exactly* when you asked it to, and you
take back the runs of a runner doing everything right. Four intervals is the
default and one interval is the floor. Fewer than four is fine; fewer than one
is the bug. `yad hub` names four intervals, and never less than 60 s, which
outlasts a runner's backoff through five failed syncs in a row.

**`lost` stands against a late result.** Once you have recorded a run `lost`,
a result that arrives afterwards is a different terminal state, refused with
`409 conflict` (§6), and the runner keeps your verdict.

**Deregister settles everything the runner held, sessions included.** A
runner that calls `deregister` is not coming back under that credential, so:
the runs it holds become `lost` (a claim you have asked to cancel,
`cancelled`, §3); the runs offered to it and not yet claimed go back in the
queue; and **every session bound to it closes, with the runs still
queued in those sessions ending**, and so does every fork of one of them
that no claim has bound (§8), which only this runner could have opened —
`yad hub` fails them with a reason that says to submit the work to a new
session. **Do not unbind the sessions
instead.** A session is resumable only on the runner that holds it, because its
transcript is on that runner's disk; offering it to another runner offers a
resume that cannot work. And do not leave them open and bound: a queued run
holds no lease, so nothing would ever end it, and it waits for good on a
runner that has gone. Then retire the credential — `yad hub` keeps the
runner's row, so a token issued for that runner id brings it back.

**A runner that stops syncing without deregistering is settled by time, in
three steps** ([0046](docs/decisions/0046-a-silent-runner-loses-its-offers-with-the-lease-and-its-sessions-after-a-day.md)).
It crashed, was switched off, or lost its network; it may come back.

- **Its held runs are lost when their leases lapse**, as above — a claim you
  have asked to cancel, cancelled.
- **An offer to it lapses with the same lease.** The `lease_ms` beside an offer
  covers the offer: one the runner has not claimed within it goes back in your
  queue, and you may offer it to any runner. A claim that arrives after that —
  the runner back, listing the run — is answered with a `cancel`, exactly as
  for a run whose lease lapsed after it was claimed: by then the run may be
  another runner's. `yad hub` also leaves that run, and every other run in its
  session, out of the offers in the answer carrying the cancel, so no runner
  is told to stop and start the same run at once, or handed a second run in a
  session it still holds one in. A claim it lists as `claimed` after its own
  lease lapsed unbinds the session it bound (§8). Conformance checks the lapse
  and the cancel.
- **Its sessions are given up after a long silence — your abandon-after.**
  Later runs in a session bound to it go to it alone, so a runner that never
  returns would leave them waiting for good. After a silence longer than your
  abandon-after, counted from the last sync you answered, close every session
  bound to it and end the runs queued in them, as deregister does — `yad hub`
  fails them with a reason naming the runner and saying to submit the work to
  a new session. **Keep the credential.** A runner that was only switched off
  syncs again and is answered normally: send it `close_session` for each of
  those sessions until it lists the session in `closed_sessions`, so it
  reclaims the workdir, and offer it new work. Pick the length yourself, but
  make it longer than your lease — shorter gives up a runner whose runs are
  still leased to it. `yad hub` uses a day (`yad hub serve --abandon-after`).

  **Do not give a runner up for silence you caused** ([0048](docs/decisions/0048-six-hub-behaviours-settled-by-zuminos-hub.md)) —
  an outage of your own is a day in which nobody could sync. That is a
  *should*, and how is yours: `yad hub` abandons nobody until it has itself
  been up for a whole abandon-after, which suits one long-lived process and
  would never fire on a hub redeployed more often than that; a hub that
  restarts on every deploy may record when it was last up and discount the
  gaps, or skip this and accept that a long outage closes sessions. What is a
  *must* is the rest of this bullet: count from the last sync you answered,
  and make the length longer than your lease. Conformance cannot wait out a
  length it cannot learn, so this one is on the not-checked list below.

**Keep accepting events after the run has ended.** A batch still in the
runner's spool when the result landed is not late, it is owed — reject it and
the record of the run is permanently short of what happened in it.

## 6. Events and results

### Events

An event is one normalised thing that happened in a run — the same kinds for
every harness, so a hub never parses a harness's own output:

| kind | carries |
|---|---|
| `text` | `text`: something the harness said |
| `thinking` | `text`: its reasoning |
| `tool_call` | `tool.id`, `tool.name`, `tool.input` |
| `tool_result` | `tool.id` (joining it to its call), `tool.output`, `tool.truncated`, `tool.is_error`, and for a shell command `tool.exit_code` |
| `status` | `status`, a short label for a phase of the run, and sometimes `text`. For people: not a closed set, and not to be parsed |
| `usage` | `usage`: tokens one model used |
| `error` | `error`: a class and message, whether or not the run carries on |

**`tool.is_error` says whether a call failed, as the harness reported it**:
`true` for a command that exited non-zero, a tool that reported an error, or
a call the owner's approval policy declined; `false` for one that succeeded.
It is **absent when the harness did not say** — a Codex web search or image
view, a status yad does not know — and never guessed from the output, so
show an absent one as neither. `tool.exit_code` is a shell command's exit
status where the harness reports one as a number: Codex does, Claude Code
does not. The output is unchanged either way — Codex's still ends `[exit N]`
or `[declined: …]` — so a hub that ignores both fields loses nothing. Both
are new within v1 and are plain optional fields, not enum values, so no
feature gates them (§11): a hub generated before them ignores them.

Tool input and output are capped at 8 KiB each, and `truncated` says when
either was cut; text, error messages and a result's final text at 1 MiB each.
These bind the runner, not you, but they tell you what to size storage for.

**Events are idempotent by `(run, seq)`, and sequences start at 1.** A resent
event is not a new one, and the first copy you stored stands. **`seq` is
one-based** — an empty stream is acknowledged through `0`, and a zero-based
implementation silently never acknowledges anything, because it treats the
runner's first event as filling a gap that does not exist.

Answer with `acked_through`: the highest `seq` you hold such that you hold
every event from 1 up to and including it, **with no gap** — `3` once you hold
1, 2 and 3. It is authoritative — the runner resends everything after it —
so returning a number you have not actually stored contiguously loses events
silently. A batch leaving a gap does not move it past the gap; the batch that
fills the gap moves it over everything already held.

**Only the runner a run was claimed by may append events to it.** Events for a
run you cannot match to the calling runner are refused: `403 not_holder`, or
`404 not_found` for a run you have never heard of. An offered run takes no
events: a run starts only once its claim is acknowledged.

**The stream is complete when `acked_through` reaches the result's
`last_seq`.** The runner sends the result only once every event is
acknowledged, but a batch can still be in flight when the result lands, so do
not treat a run's record as final the moment its result arrives. A run you
recorded `lost` has no result; its stream is whatever you hold.

### Results

**Results are a wider door, and deliberately so: accept one from the runner the
run was offered to *or* claimed by.** That is not sloppiness about who owns a
run — it is how a runner **refuses** one. A run it will not take is reported as
a `failed` result with error class `refused`, and it is reported *before* the
run is ever claimed.

A hub that demands a claim first never hears the refusal. The runner simply
does not list the run, so you take the offer back and queue it again — and
offer it again, and take it back again, for as long as that runner is the one
you reach for. Declining silently by omitting a run from the next sync is the
runner's other way out and is normal; the refusal is what tells you *why*, and
tells you not to try again. A refused run is a failed run: record it so, and
never offer it again. Conformance checks both halves — it refuses a run it was
only offered, and wants the result taken and the run not offered after.

**"Offered to" means offered to now**
([0047](docs/decisions/0047-four-v1-protocol-rules-settled-by-the-clean-room-check.md)).
Take a result from the runner a run is offered to from the answer that
offered it until the offer closes: the next sync that leaves the run out,
which takes the offer back (§3), or the offer's lease lapsing (§5) — lapsed
is lapsed, whether or not you have yet put the run back in your queue. After
that, refuse it with `403 not_holder`: the run may be another runner's by
then, and no runner may fail a run another has. An offer made again, to the
same runner, opens the door again. Conformance checks it: it leaves an offer
out of a sync declaring no free capacity, then refuses the run, and wants
`403 not_holder`.

A yad runner sends its refusal straight after the sync whose answer carried
the offer, before it syncs again, so the refusal lands while the offer is
open. If that attempt fails, it tries again after its next sync, which by
leaving the run out has taken the offer back. If that sync's answer offered
the run to it again, the retry lands; if not, the hub answers `403
not_holder`, the runner drops the refusal, and the next offer of the run to
it is refused afresh. A refusal can be lost that way, and the run offered
once more — never failed by a runner that no longer has it.

A `refused` result may also come for a run the runner has claimed — nothing
about the class ties it to an unclaimed run, and the conformance suite sends
one for a run it holds. Apply it as any terminal state.

**Results are idempotent, and applied at most once.** The same terminal state
again is acknowledged. A *different* terminal state is `409`, and yours
stands: a runner reporting `succeeded` for a run you already recorded `lost`
is told so and stops. That holds for a run you ended before any runner
claimed it too ([0048](docs/decisions/0048-six-hub-behaviours-settled-by-zuminos-hub.md)): a refusal arriving for a run you
cancelled while it was offered is `409`, and the run stays `cancelled`. A
`403 not_holder` there is as final to a runner, and is also conforming.

A result from any other runner is refused, `403 not_holder` or `404
not_found`, and never applied.

### How a runner treats your answers to events and results

This is what makes the statuses matter:

| your answer | events batch | result |
|---|---|---|
| 2xx | the spool is trimmed through `acked_through` | done |
| `409 conflict` | retried | done: your state stands |
| a 4xx other than `413`, with code `not_holder` or `invalid` | dropped: they stay only in the runner's local spool | dropped |
| `413`, whatever its code | sent again in halves, down to one event at a time | retried |
| anything else — `401`, `404`, a `5xx`, a proxy's bare 4xx | retried | retried, backing off to once every 5 minutes, and replayed at every runner start |

So a `404` is never final — it may be a wrong connection URL rather than an
unknown run — and a refusal you mean to be final must say `not_holder` or
`invalid`. The `413` row comes first: a size limit says nothing about the
report, so the status wins over any code on it.

## 7. Controls and features

**Controls are not acknowledged.** Nothing in the protocol tells you a control
arrived — the only evidence is what the runner does next. So each control has
a rule for when to stop sending it:

| control | carries | what the runner does | send it |
|---|---|---|---|
| `cancel` | `run_id` | ends the run: interrupts the harness, then signals its process group, then kills it. A waiting run ends where it stands. A run whose claim you answer with a cancel never starts, and no result is owed: the runner leaves it out of its next sync, and you record it `cancelled` (§3) | in every response to a sync that lists the run, until the run ends. The runner acts on the first |
| `interrupt` | `run_id` | ends the current turn and keeps the session; the run ends `cancelled` unless the turn finished first. An interrupt that reaches a run before its harness is up ends it as a cancel does | the same as `cancel` |
| `steer` | `run_id`, `text` | hands the text to the running harness as more input, at its next tool boundary or after the turn. One the harness would not take appears as an `error` event with class `steer_failed` | **once**. Repeat it and the harness reads the text twice |
| `close_session` | `session_id` | closes the session and deletes its workdir; a session with a run held closes when that run ends; one it does not hold or already closed is reported closed | in every response until the session appears in the runner's `closed_sessions` (§8) |
| `drain` | nothing | stops claiming, lets the runs it holds finish, then exits. Its health says `draining` and its free capacity is zero | in every response until a sync's health says `draining` |
| `report_capabilities` | nothing | sends its capability document in the next sync | while the fingerprint differs from the document you hold |
| `start_login` | `login_id`, `harness`, `account` (absent: the harness's own default login), `add` | runs the harness's own login in that account's home and reports its link as `url` while the login is `waiting` — for Codex with `user_code`, the device code to type there. With `add`, the account is new, and the runner lists it once the login takes | in every response until the runner reports the login in `logins` |
| `login_code` | `login_id`, `code` | writes the code the owner got at the link to the login, which then moves to `checking` | in every response while the runner reports the login `waiting` with no `user_code`; never for one with a `user_code`, whose code is typed at the link |
| `login_token` | `login_id`, `harness`, `account`, `token`, `add` | stores a `claude setup-token` token as the account's login; with `add`, as `start_login` | in every response until the runner reports the login — then forget the token |
| `cancel_login` | `login_id` | ends the login `cancelled`; one it never had is reported `cancelled` all the same | in every response until the runner reports the login over |
| `remove_account` | `harness`, `account` | removes the account as its owner's `yad account remove` does: listed nowhere from then on, a run on it finishes there, its home is deleted when the last one ends, and a login in flight on it ends `cancelled`. One it does not list is nothing to do | in every response until neither the runner's current capability document nor the sync's health lists the account, or the runner stops advertising `accounts` |
| `update` | — | reserved; never send it. A yad runner ignores it: it updates itself only when its owner turns that on ([0071](docs/decisions/0071-a-runner-updates-itself-when-its-owner-turns-it-on.md)) | never |

**Features are promises, not decoration.** A runner advertises
`protocol_features` in its capability document. They exist so a hub does not
send a control that would be silently ignored — nothing acknowledges a
control, so an ignored one is indistinguishable from an obeyed one to whoever
asked for it.

| feature | what it gates |
|---|---|
| `steer` | the `steer` control |
| `interrupt` | the `interrupt` control |
| `drain` | the `drain` control |
| `close_session` | the `close_session` control. A runner advertising it also reports every close in `closed_sessions`; one that does not advertise it may report closes too, and each is believed all the same (§8) |
| `start_at` | a run carrying `start_at` — offer one only to a runner that advertises it |
| `effort` | a run carrying `effort` — offer one only to a runner that advertises it, however long it waits |
| `fork` | a run carrying `session.fork_from` — offer one only to the runner holding the session it forks, and only while that runner advertises it (§8) |
| `login` | the four login controls. A runner advertising it reports each login in `logins` |
| `accounts` | `add` on `start_login` and `login_token`, and the `remove_account` control. Per hub: a runner advertises it only to a hub its owner lets add and remove accounts, and only beside `login`, so its fingerprint for you may differ from another hub's |
| `live_sessions` | a run whose `session.mode` is `live` — offer one only to a runner that advertises it. The spec lists `live` as an enum value and connects it to no feature, so this pairing exists only here |
| `harness_features` | nothing by itself: it says each harness report carries `features`, the per-run features a run on that harness may use, and that those lists — not the runner-wide strings — answer for `steer`, `interrupt`, `effort` and `fork` |

**`steer`, `interrupt`, `effort` and `fork` are the harness's.** Whether a
run may use one depends on the harness it targets: a harness driven one way
may fork and not take a steer, another the reverse. A runner advertising
`harness_features` lists each harness's in its report's `features`, and then
that list is the whole answer for runs on that harness — absent means none,
and a harness the document does not list takes nothing. Gate a run's `effort`
and `fork_from`, and a `steer` or `interrupt` for a run, on the list of the
run's `harness`. The runner-wide strings stay, for a hub that does not read
the lists: a yad runner lists one there only while every harness it drives
supports it, so a hub going by them alone is never wrong, only more cautious
than it needs to be — it holds back a Claude run's steer because another
harness on the runner takes none. Without `harness_features`, the runner-wide
strings are the answer for every harness
([0069](docs/decisions/0069-a-per-run-feature-is-its-harnesss.md)).

**A feature gates what you send, never what you accept** ([0048](docs/decisions/0048-six-hub-behaviours-settled-by-zuminos-hub.md)).
Whatever a runner reports — a close, a state, an event — is taken on its own
terms whether or not the runner advertises the feature it came with.

Only `cancel` and `report_capabilities` go to every v1 runner. Send a gated
control to a runner that does not advertise its feature and nothing happens,
for ever. `yad hub` answers the person asking for one with `409` rather than
queueing it, and keeps a control it has already queued, rather than spending
it, when the runner's document stops advertising the feature or is known to
be out of date.

**`start_at` is a moment, not a schedule.** A runner advertising `start_at`
claims such a run, holds its capacity, and starts it at that moment, so you
may offer it early. Once the moment has passed there is nothing to hold, and
the run may go to any runner; otherwise it would wait for ever on a fleet
without the feature. There is no recurrence anywhere in the protocol: a
schedule is yours, and it makes runs.

**`effort` is the harness's word, not the protocol's.** It is a string, like
`model`, and not an enum: the levels are each harness's own, they differ by
model, and they grow with harness releases. Claude Code takes `low`, `medium`,
`high`, `xhigh` and `max`; Codex takes the reasoning levels its model lists —
`low` to `xhigh` for most, `max` or `ultra` for some; OpenCode the effort
variants its model has, `low` to `high` for many and none for some, whose run
then fails before anything is asked. A runner checks no name,
only the shape §4 gives, and hands the word to the harness: Claude's `--effort`, the `effort` of
Codex's `turn/start`, and OpenCode's effort option over ACP. A level the harness does not take fails the run with
class `harness_error` and the harness's own words — Claude warns and would
carry on at its default, so the runner stops it before it starts working and
fails it with that warning. Absent, the harness uses its default, on every run: a
run continuing a session does not inherit the effort an earlier run in it
set. Gate it because a runner without the feature drops a
field it does not know and runs the harness at its default, and the run
succeeds with nothing to say it was not what you asked for. A runner that
advertises `effort` but is handed one for a harness whose effort it cannot set
refuses the run, class `refused`. A hub gating on that harness's own list (`harness_features`) never sends it one.

**Hub login** ([0055](docs/decisions/0055-a-hub-may-log-an-account-in-by-link-or-by-token.md))
lets your UI log a runner's account in without a shell on the machine: by
**link** — `start_login`, show the `url` the runner reports, take the code the
owner pastes and send it with `login_code` — or by **token** — `login_token`
with a `claude setup-token` token the owner pasted. Choose the `login_id`;
it is 1–128 letters, digits, dots, dashes or underscores. The account is a
label the runner lists, which its health names, and a login for any other
ends `failed` — unless it carries `add` (below). A token needs an account; a
link login without one logs in the harness's own default login.

**Codex logs in by device code**
([0057](docs/decisions/0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md)),
still a `link` login: its `waiting` report carries `user_code` beside `url`.
Show both; the owner signs in at the link and types the code there, and
nothing comes back through you — **never send `login_code` to a login that
reported `user_code`**. It ends like any other, by the runner's own check.
Codex takes no token: a `login_token` for it ends `failed`.

Each login's `state` moves `starting` → `waiting` (`url` set) → `checking` →
one of `succeeded`, `failed`, `expired`, `cancelled`, and the runner repeats
it in every sync until one carrying the end is answered — take a repeat as the
same news, and a report for a login you never started as news you may ignore.
`error` says why a login did not take, in the runner's words with the next
action. Success is the harness's own login check on the machine, never its
output. A newer login for the same account replaces the older one, which the
runner reports `cancelled`. yad's runner gives a code ten minutes (`expired`
after), whether it is pasted back or typed at the link.

**Managing accounts** ([0057](docs/decisions/0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md))
lets your UI add a runner's account and remove one, where the runner
advertises `accounts` to you. Offer both only then; a re-login needs only
`login`.

- **Add** is a login carrying `add: true`, followed in `logins` exactly as
  any login. `account` is the new label: lowercase letters, digits, `-` and
  `_`, starting with a letter or digit, at most 64. The runner lists it only
  once the login takes, so `succeeded` means the account is now in its health;
  a login that ends any other way lists nothing. A label the runner already
  lists, one it does not take, none at all, or a hub the owner has not let
  add accounts, ends `failed` with the next action. When a harness's health
  names no accounts, it runs on its own default login, and the first account
  added replaces that login in rotation: say so before the owner adds it.
- **Remove** is `remove_account`, repeated until neither the runner's
  current capability document nor the sync's health lists the account, or
  the runner no longer advertises `accounts`. The document decides: it names
  every harness the runner found with all its accounts, and the runner sends
  a new one after a removal. Health cannot say an account is gone — it names
  only the harnesses the runner can drive, so removing a harness's last
  account on a machine with no default login takes the harness out of it
  altogether, and it names at most sixteen accounts of a harness — but while
  it still lists the account, a document not yet rebuilt does not end the
  removal. Removing a harness's last account
  puts it back on its own default login. A login that adds the account again
  ends a removal still waiting, or the removal would go out once the account
  is back and take it away.
- An account is the machine's, not the hub's that added it: every hub the
  runner syncs with has its runs rotate through it, and sees it in health.
  There is no ownership to show, and a hub may remove an account the owner
  added at the machine.

**The rules the reports cannot enforce for you.** The `code` and the `token`
are secrets: never log them, never show them again, never return them from an
API. Hold a token **only until a sync reports the login it was delivered for**
— the runner's answer to the sync that carried it — and blank it then (`yad
hub` does it in a trigger, and opens its database with `secure_delete`); a
login that never reaches its runner should expire and lose its token too.
Both deadlines below end a login holding a token, so none is held for ever.

**End a login on your own word only while you have never sent it.** Record
when an answer first carries its `start_login` or `login_token`. Until then a
cancel, a newer login for the account, or your own deadline may end it on the
spot. After that, no report yet does not mean the runner has not got it: it
may already have stored the token, or the login may have taken. Send
`cancel_login` instead and wait for the runner to say how it ended; if an end
you wrote yourself still meets a runner's report of one, the runner's wins.

**A sent login unheard for thirty minutes MUST end.** A runner that took the
answer and then never syncs again — the machine gone, crashed and never
deregistered — would otherwise leave the login open and its token held for
ever. Thirty minutes from the start is past every deadline a runner holds a
login to (ten for the code, a minute for the rest) with room for syncs: end
it `failed` then, and blank its token and its code. `yad hub` does it in its
sweep (`loginFinishWithin`). If the runner does report an end afterwards,
its report replaces yours, as above.

And a login the runner reported and then **leaves out** of a sync while it
was not over is gone — the runner restarted, and a login in flight does not
survive that — so end it `failed` rather than show it waiting for ever.

**A draining runner is offered nothing.** Stop offering to a runner as soon as
you have decided to drain it, not only once its health says `draining` — an
offer made in between would only come back.

## 8. Sessions

A session is a durable conversation with one harness in one working directory,
on one runner. You choose its id; the runner maps it to the harness's own
session id, so you never need to know what a transcript file is.

**`session.new` is `true` while no claim has bound the session, and `false`
after** ([0047](docs/decisions/0047-four-v1-protocol-rules-settled-by-the-clean-room-check.md)).
Decide it when you offer the run, not when it is submitted: the run that
opens a session is the first one a runner claims, and you learn which that is
only from your own history. A session whose first run was refused, cancelled
on its claim, withdrawn or left out by the runner was bound by no claim, and
exists on no runner — so its next run goes out with `new: true`. Once a claim
has bound the session (§3), every later run in it goes out `false`. A runner
refuses `new: true` for a session id it already has, and `new: false` for one
it does not hold, rather than guess: guessing would resume a conversation that
does not exist, or silently start over one that does.

A later run names the same `sources` as the first, or none — and a runner
builds a new session's workdir from the run that opens it. So when a run that
named none goes out with `new: true`, because the session's first run never
bound it, send it with the sources that first run named. A continuing run
needs nothing added: the runner holds the session's sources.

What a runner keeps matches this. A refusal creates no session. A claim you
cancel before its listing is answered, or one the runner withdraws, takes the
session it opened with it (§4). A claim held by a runner process that stopped
before the claim was answered is withdrawn by the next process, session and
all; one that was answered — so you bound the session — and had not begun to
prepare is reported `lost`, and its session stays for the next run to
continue.

An answer you sent that the runner never read leaves you with a bound session
the runner does not have. You can see it in two ways (§3):

- **a cancel you sent next reached it first.** The sync that leaves that
  cancelled claim out is the runner saying it withdrew the claim, and with it
  the session the claim opened;
- **the runner was silent past the claim's lease, and lists it as `claimed`
  when it comes back.** You ended the run when the lease lapsed — `lost`, or
  `cancelled` if you had been asked to — so your answer is a `cancel`, and a
  claim still listed as `claimed` is one no answer acknowledged: the runner
  withdraws it at whichever cancel it hears first, and the session with it.
  A run it lists as `preparing` or later started, and it stops that run and
  keeps the session. A run with a start moment is the exception: the runner
  holds an acknowledged claim as `claimed` until its moment, so the listing
  cannot tell a lost answer from a kept session, and the session stays bound.

So **keep which run's claim bound each session, and unbind the session when
the withdrawn claim is that run** — at the sync that leaves it out, or at the
sync that answers its late listing with a cancel — and its next run then goes
out `new: true`, to any runner. Nothing else unbinds a session. A withdrawn
claim that did not bind it came after a run that did, and the runner keeps a
session that has run a turn; a session whose close you asked for is closed by
the runner, not deleted, and you are waiting to hear so; a lease that lapses
with no sync after it cannot tell a withdrawal from a runner gone silent.
`yad hub` does exactly this. The one case neither side can see is the runner
stopping before it read that answer: you bound the session, the next process
withdrew it, and the next run, sent `false`, is refused as a session the
runner does not hold. A claim with a start moment whose answer was lost is
the same, seen from the hub.

**Sessions stay put.** The first claim in a session binds it to that runner.
Its later runs are offered to that runner alone — a session is resumable only
where its transcript is — and **one at a time**: do not offer a run in a
session while another run in it is offered or held. A runner refuses a second
live run in a session.

**Closing.** A session closes by your word (`close_session`, where the runner
advertises it), by its owner's (`yad sessions close` on the machine), when it
has been idle past the owner's time-to-live, or under disk pressure. It closes
only while no run is held in it, and takes no new run afterwards. Every close
the runner makes is reported in `closed_sessions`, with a `reason` — `closed`,
`closed_by_owner`, `expired` or `disk_pressure` — in every sync until one
carrying it is answered with a 2xx. So:

- record closes by session id, and take a repeat as the same news — from a
  runner that does not advertise `close_session` as well (§7);
- stop offering runs in a closed session: the runner refuses them with class
  `session_closed`;
- **offer nothing in a session you have sent `close_session` for** and not yet
  heard closed ([0048](docs/decisions/0048-six-hub-behaviours-settled-by-zuminos-hub.md)). The runner acts on the control before
  the offers beside it, so an offer made while the close is out is refused as
  `session_closed` — a failed run where your close meant a cancelled one.
  Hold the session's queued runs back; they end with it when the close is
  reported, as below;
- **end the runs still queued in it**, rather than offer them to be refused.
  `yad hub` cancels them when the close was yours (`closed`), and fails them
  otherwise, with a reason saying to submit the work to a new session;
- answer the sync normally. The report is repeated until a sync carrying it is
  answered, not until it is believed.

A session you close before any runner has claimed a run in it is yours alone
to close: there is nothing to send.

**A reported close is the same kind of wider door: believe it from the runner
holding the session, *or* from the runner its run was last offered to while no
claim had bound it.** A runner records an offered run as claimed, and opens
the session with it, before any sync has listed that claim. If the claim is
then withdrawn — you cancelled it, the runner began draining, it stopped or
restarted — and the runner's owner had asked to close the session meanwhile,
the session closes on the runner and arrives in `closed_sessions`, about a
session you never saw bound. A hub that believes closes only from a bound
runner drops that report, offers the run again, and hears it refused as
`session_closed`; a run queued behind it gets the same. `yad hub` remembers
which runner a session's run was last offered to until a claim binds it,
closes the session on that runner's report and binds it to that runner, and
ends every run still waiting in it — as it does on any close a runner reports.
A close from any other runner changes nothing: it has no claim to the session,
and a session is closed only by the one whose disk it would be on. Nor does a
close of a session you never heard of: `yad hub` ignores both, and answers the
sync that carried them as it would any other.

**Forks** ([0065](docs/decisions/0065-a-fork-is-a-new-session-opened-from-another-sessions-conversation.md)). A run with `session.new: true` may name
another session in `session.fork_from`: the new session's conversation starts
as the harness's copy of that one's, as far as the harness has written it,
and the two diverge from there — the session forked goes on resuming its own
conversation, untouched, and may even have a run live while it is forked.
The fork is otherwise a new session like any other: its own id, its own
workdir built from the run's own `sources` (nothing of the forked session's
workdir comes with it — name the same repository on a branch of its own if the
fork should see the same code), bound by its claim.

- Offer the run only to the runner the forked session is bound to, and only
  while it advertises `fork` for the run's harness (§7). A fork of a session no claim has bound has
  no conversation anywhere to copy; `yad hub` refuses one at submit.
- Send `fork_from` on whichever run opens the session — decided at offer, as
  `session.new` is: if the fork's first run never bound it, its next run goes
  out `new: true` with the same `fork_from` — and on no later run.
- The runner refuses the run, class `refused`, for a forked session it does
  not hold for your connection, one of another harness, or one closed or
  closing. A forked session with no conversation yet, or one whose transcript
  the harness cannot find, ends the run `resume_rejected`, as a resume does.
- When the forked session's runner goes, close the forks of it no claim has
  bound, with its own sessions: no other runner can open them.
- **Never leave a fork waiting on a session that can no longer be forked**
  (DEV-151). When you unbind the forked session — its binding claim was
  withdrawn (above), so the runner deleted it before any turn ran — it has
  no conversation to copy; when you ask to close it, or a runner reports it
  closed, the runner would refuse the fork. Either way the fork can go to no
  runner, and a queued run holds no lease for anything to end it. `yad hub`
  closes each fork of it no claim has bound, at that moment, and fails the
  runs waiting in them with a reason saying the source has no conversation to
  copy — 0065's `resume_rejected` end, in words, since a run a hub ends
  carries no class. It does not wait for the source to be bound again: a
  later run there opens a new conversation, not the one the fork was asked of.
  A fork a claim has bound has its own conversation, and is left alone —
  until you unbind it because that claim was withdrawn: then it has none,
  and if its source can no longer be forked it closes the same way.

**A session whose runner has gone** — deregistered, or silent past your
abandon-after — is covered in §5: close it and end its queued runs; never hand
it to another runner. A runner that comes back after abandon-after is sent
`close_session` for each such session until it reports the close.

## 9. Grants

A grant is a short-lived secret you attach to one run — a token for your own
API scoped to that run's work. The runner delivers it to the harness process
alone, never in the prompt, the logs or the events: `as: "env"` sets
`NAME=value` in the harness's environment; `as: "file"` writes the value to a
`0600` file outside the checkout and sets `NAME` to that file's path. It is
deleted when the run ends. The naming rules are in §4, and a grant is for the
*work*: it may not name a variable that would change which account the
harness spends.

**Hold a grant only while its run can still use it.** A grant is a secret,
sent to you for one run. `yad hub` keeps it in the run's stored spec until the
run reaches a terminal state — `succeeded`, `failed`, `cancelled`, `timed_out`
or `lost` — and then blanks every value and keeps each grant's name and
delivery, so the record still says what the run was given. A `waiting` run is
not terminal and keeps its values, because its resume is built from them. It
does this with a database trigger rather than in each code path that ends a
run, because the next way a run ends will be written by someone who never
thought about grants
([0041](docs/decisions/0041-a-hub-holds-a-grant-only-while-its-run-can-use-it.md)).
Nothing checks this from outside, so it is yours to get right.

**Grants never touch the runner's disk except as the run's own file grants.**
A run parked on a usage limit keeps its grants in the process that claimed
it; if that process dies, the run is reported `lost` with class `grants_lost`.
Submit a new run with grants your submitter gives you — the old values are
already blanked.

**A credential in a git source's URL is a grant too.** An `https` URL may carry
one in its userinfo — `https://<token>@host/org/repo`, or `user:password@` —
and the runner fetches that repository with it, for that run alone: it keeps
the URL without it in its cache, its store, its log and the run's events, and
gives it to git only in the environment of the run's fetch, never argv. A user
with no password is offered as a token first; if the remote refuses it, the
runner's own credential helpers are asked for that user's name, so a clone URL
that names the account, as Azure DevOps and Bitbucket hand out, still works. A later
run in the session may send another credential or none and still names the
same source; one without it is fetched with the machine's own credentials. Like
a grant, it does not survive the runner restarting while the run waits: send
it again with the run the `grants_lost` result asks for. An `ssh` URL's user is
its login name, and one with a password is refused
([0068](docs/decisions/0068-a-credential-in-a-source-url-is-the-runs-alone.md)).

## 10. Errors and `next_action`

**Every error is the envelope, and `next_action` is mandatory.**

```json
{"error": {"code": "invalid", "message": "...", "next_action": "..."}}
```

`message` says what went wrong and `next_action` what to do about it, both for
a person: a runner on someone else's machine is debugged by whoever reads
them. Every error response under your base path carries this envelope — an
unknown path and a wrong method included. A plain-text 404 is a protocol
violation.

v1 names ten codes. They are not a closed set — the spec types `code` as a
plain string — so a runner meeting one it does not know goes by the status and
shows `message` and `next_action` to a person.

| code | means | `yad hub` sends it with | a runner that receives it |
|---|---|---|---|
| `unsupported_protocol` | `Yad-Protocol` is missing or names another version | `426` | stops syncing your hub |
| `version_too_old` | the runner is below your `min_version` | `426` | stops syncing your hub |
| `unauthorized` | no bearer, a token or credential you did not issue or no longer honour, or one belonging to another runner | `401`, or `403` for another runner's credential | on sync: stops syncing your hub. On events and results: retries |
| `runner_revoked` | a credential you have revoked | — (`yad hub` says `unauthorized`) | stops syncing your hub |
| `not_holder` | events or a result for a run the calling runner does not hold | `403` | drops what it sent for that run |
| `not_found` | a run you have never heard of, or a path that is no operation | `404` | retries: a `404` may be a wrong URL |
| `conflict` | a result differing from the terminal state you hold; a known runner id with a token for a new runner | `409` | on a result: keeps your state and stops |
| `invalid` | a body that does not validate, or a wrong method | `400` — never `422` — or `405` for a method, and on a `413` for a body too large | on events or a result: drops it, except under a `413` (§6). On sync: backs off and retries |
| `internal` | a fault on your side | `5xx` | retries, backing off |
| `not_implemented` | an operation your hub does not serve | — | goes by the status |

**`internal` is a fault on your side**, sent with a `5xx`: the request may have
been fine and the same one later may succeed. A runner treats it as any `5xx`
— the sync loop backs off and tries again, and events and a result stay
spooled and are sent again until you take them — so it never stops a runner
and never loses a report. That is exactly why it must not stand in for a
refusal: a request you will never accept, sent back as `internal`, is retried
for ever. Send the status that is true and a `next_action` a person can act on;
that pair is what a runner's operator reads.

**The envelope is required; most of the status numbers are yours.** The
protocol fixes only a few: `426` for a missing or wrong `Yad-Protocol`, `409`
for a terminal state that differs from one you hold, and `403`/`404` for a
call about a run you cannot match to the caller. For everything else — a spent
registration token, a credential that belongs to another runner, a wrong
method — what is checked is that the envelope and its `next_action` are there,
not which number carries them. Pick sensibly and be consistent; nothing will
fail you for choosing `401` over `403`. The codes are what a runner acts on,
so the code matters more than the number: a refusal you mean to stop a runner
must carry one of the codes above that stops it.

**Write `next_action` for the runner's owner, who may not be at your hub.** A
good one names the command to run and on which machine. It is shown as you
wrote it; keep secrets out of it, and out of `message` — nothing on the
runner's side can tell a secret in your error text from any other word.

## 11. Versioning

**The major version is in the path and in the header.** A runner sends
`Yad-Protocol: 1` on every request; that is the only value a v1 hub accepts,
and `openapi.yaml` declares it on every operation as a required header whose
only value is `"1"`, beside the `426` that refusing it produces. A future v2
is a new document and a new header value, never an edit to this one: within
v1, a field is never renamed or removed.

**Within v1, things are added, and both sides tolerate it.** Unknown fields
are ignored (§2); unknown error codes go by the status (§10); unknown error
classes and status labels are shown rather than parsed; unknown
`protocol_features` strings are ignored.

**Enums do not grow unasked.** Every enum in the document — event kind, run
state, result state, close reason, control kind and the rest — is closed for
all of v1
([0047](docs/decisions/0047-four-v1-protocol-rules-settled-by-the-clean-room-check.md)).
A value added to one arrives only behind a feature the receiving side
advertised: a new control kind goes only to a runner advertising a feature for
it in `protocol_features`, and a new event kind, run state, result state or
close reason goes only to a hub advertising one in `hub_features`. So an older
side never sees a value it cannot validate, and may refuse one it was sent
anyway as `invalid`.

The three enums you send, a control's `kind`, a grant's `as` and a session's
`mode`, appear in the document as `x-extensible-enum` rather than `enum`
([0058](docs/decisions/0058-an-enum-a-hub-sends-is-written-as-x-extensible-enum.md)).
The values listed are still the whole set as of the document you hold. The
spelling says that the set grows within v1 behind a runner's
`protocol_features`, and it keeps a breaking-change check from rejecting that
growth. A generator that does not read the extension types these fields as
plain strings. Every enum a runner sends you stays `enum`, and you may
validate it strictly.

**Features, both ways.** The runner advertises `protocol_features` in its
capability document; the hub may advertise `hub_features` at register. Nothing
is used that the other side did not advertise (§7). v1 defines no hub
features, so a yad runner sends only the values the document lists. A runner
whose fingerprint moved without the document it promised is treated as
advertising nothing until the document arrives.

**`min_version` refuses old runners, if you want to.** Answer `min_version` in
register and sync responses, and refuse a runner below it with
`version_too_old` and a next action that says to upgrade. Refuse at register
*before* spending the token, so the upgraded runner can reuse it; and at every
sync, judged on the `yad_version` of the newest capability document — the
one the sync carries, or else the one you hold. A
runner refused mid-run stops syncing, so the runs it holds stop renewing and
are recorded `lost` — raising the floor on a working fleet is a drain first.
Compare versions on the release core alone (`0.4.0` in `v0.4.0-4-gabc1234`,
which is a build four commits *after* v0.4.0), and never refuse a version you
cannot parse — an unstamped `dev` build, or a mistyped floor.
`yad hub serve --min-version 0.4.0` is `yad hub`'s floor.

## 12. Proving it: `yad conformance`

```
yad conformance <connection url> --token <token> [--second-token <token>] [--harness id] [--lease-wait d]
```

Forty-nine black-box checks against a URL, written from the protocol rather
than from `yad hub`'s internals — nothing in the suite imports the hub, so it
tests the protocol and not one implementation of it. A failure gives you the
rule as a sentence and the section of this page that states it.

The URL is your connection URL — every path is relative to it, so a hub
mounted at `https://example.com/yad/v1` is named in full. Each token is a
registration token your hub issued for a **new** runner and nobody has used;
`-` reads one from standard input, and only one of the two can be `-`. The
command exits 0 when nothing failed and 1 otherwise.

**What it does to your hub.** It spends the registration token you give it and
registers a runner advertising a harness no real run asks for —
`yad-conformance`, or `--harness` — so a suite pointed at a live hub is offered
nothing anyone was waiting on. It advertises none of the runner's own
features, and `steer`, `interrupt`, `effort` and `fork` runner-wide beside
`harness_features` and an empty list for its harness: runs on it may use none
of them, and a hub going by the runner-wide strings alone is caught offering
one (§7). No yad runner sends that pairing; it is there to be read.

**Queue at least three runs for that harness first, and be able to offer two
at once.** The lease rules need two runs held simultaneously — one to report on,
one to leave unrenewed until its lease lapses — so one run is not enough, and
**two offered one at a time is also not enough**. Offering one run per sync
breaks no rule in §2 and is a perfectly good hub; it simply leaves those checks
unreachable, and the suite says so rather than failing you. The third is left
offered and unclaimed until its offer's lease lapses. More than three does no
harm. The suite reports one run `failed` with class `refused`, leaves one held
run to lose its lease and the offered one to lapse. It takes that third one
once more, leaves it out of a sync so the offer is taken back, and sends a
refusal too late, which you must refuse. Then it refuses the third run the way
a runner declines one — a `failed` result, class `refused`, without ever
listing it — while it is offered, so all three end `failed` or `lost`. `--lease-wait`
(default 90 s, `0` skips the lease rules) bounds how long it waits, and must
outlast the `lease_ms` you name.

It also sends one events batch and one result of 17 MiB each — the first
event again and the terminal state again, padded with a field no version
defines — to see how you refuse a body for its size.

Without them those checks are **skipped**, and each skip says what to queue to
make it possible.

**Give it a second registration token to check who holds a run.**
`--second-token` spends it on a second runner, which sends a batch of events
and a result for the run the first runner holds. Both must be refused with
`403 not_holder`, the code a runner stops on, or `404 not_found` if your hub
will not name a run to a runner that does not hold it; any other refusal fails,
because a `5xx` or a `401` sends the runner back to retry what you will never
take. The same runner then syncs with its own credential on the first
runner's path, which must be refused. Without the second token those three
checks are skipped, saying which flag would make them possible;
the half that needs one runner — a run you cannot match to the caller at all
is refused — is checked either way.

**A skip is not a pass.** It is the suite saying your hub gave it no way to
ask. Read the skips before believing the passes.

Against `yad hub` itself, as an example — three runs queued for the
conformance harness, then both tokens:

```bash
yad hub serve &                      # 127.0.0.1:7878
yad hub admin-token create
yad hub submit --harness yad-conformance --model any "conformance run 1"
yad hub submit --harness yad-conformance --model any "conformance run 2"
yad hub submit --harness yad-conformance --model any "conformance run 3"
yad hub token create > first.token
yad hub token create |
  yad conformance http://127.0.0.1:7878/v1 --token "$(cat first.token)" --second-token -
rm first.token
```

It ends `49 passed, 0 failed, 0 skipped`, after about a minute spent waiting
out a lease.

### What it does not check

The suite prints this list itself, and it is seventeen rules — not a footnote. Each
is something **your hub still has to get right** with nothing to catch you:

| rule | why the suite cannot reach it |
|---|---|
| `POST /runners/{runner}/deregister` — held runs lost (a claim you cancelled, `cancelled`), offers requeued, the runner's sessions closed and their queued runs ended | deregistering retires the runner every other check is made as; the second runner `--second-token` registers could carry it, and does not yet. `yad hub` implements it; implement it in yours |
| The controls — `cancel`, `interrupt`, `steer`, `close_session`, `drain` — their repetition until the runner acts, a `steer` being delivered once, a cancelled claim the runner withdraws recorded `cancelled` rather than `lost` and the session it bound unbound, and nothing offered to a runner draining or asked to drain | nothing in v1 lets a *runner* ask for a control, so the suite can only wait for one it cannot cause |
| `start_at`, `min_version`, the feature gates on `drain`, `steer`, `interrupt`, `close_session`, `start_at`, `effort`, `fork`, and holding every gated control and run back while a moved fingerprint's document has not arrived | each needs a run or control the protocol gives a runner no way to request. The other half *is* checked: that a hub sends no control it should have gated, and asks with `report_capabilities` when the fingerprint moves |
| Hub logins — `start_login` and `login_token` repeated until the runner reports the login, `login_code` while it reports it `waiting`, `cancel_login` until it reports it over; a token held only until the runner reports its login; a login the runner reported and then leaves out ended `failed`; a login ended on the hub's word only while never sent, and a sent one unheard for thirty minutes ended `failed` with its token blanked, a runner's later report still replacing that end | only your own API starts a login, outside v1, and the suite advertises no `login` feature to be sent one. What is checked: that no login control reaches it, and that a sync carrying `logins` is taken |
| Adding and removing accounts — `add` and `remove_account` only while the runner advertises `accounts` to you, and `remove_account` repeated until neither its capability document nor its health lists the account, or `accounts` is no longer advertised | only your own API adds or removes an account, outside v1, and the suite advertises no `accounts` feature to be sent either. What is checked: that neither reaches it |
| Sessions staying put — first claim binds the session to that runner, later runs to that runner alone, one at a time — and `session.new` set right | needs two runs in one session, which only your own queueing can arrange |
| Closes in `closed_sessions` believed from the holder or the last-offered runner, whatever features it advertises, a repeat taken as the same news, and the runs queued in a closed session ended; nothing offered in a session you have sent `close_session` for until the close is reported | the suite advertises no `close_session`, so no hub asks it to close a session, and the only sessions it has hold its runs, which no runner closes; the queued runs need a second run in the session |
| Offers only for a harness the runner can drive — first-class, present, no `error` — and preferably one whose health says `ready` | the suite is offered only what you queued for the one harness it advertises; seeing another offered needs a run queued for it |
| Lapsed leases found by a timer as well as by a sync | anything the suite sends to find out is itself a request you could settle leases on, so a hub that settles them only when asked looks the same |
| A known runner id re-registers only with a token issued for that runner | such a token comes from your own API, outside v1; trying the second token on the first runner's id could spend it before the runner it is for |
| Grant values kept only until the run is terminal (§9) | v1 gives a runner no way to read a run back |
| `internal` only with a `5xx`, never for a request you will never accept | a fault cannot be caused from outside. What is checked: the refusals the suite can cause — an invalid body, a run the caller does not hold, a body too large |
| A runner silent past your abandon-after — its sessions closed and their queued runs ended, its credential kept, `close_session` sent when it returns | the silence is a day by default and every hub names its own, which v1 gives a runner no way to ask |
| Capacity shared round the hubs, one pool, a unit at a time | a rule about one runner across several hubs, so no single hub can pass or fail it |
| The caps on tool output, event text and final text, and halving a batch a proxy refused | those bind the runner, not the hub |
| Whether `register` refuses a missing or wrong `Yad-Protocol`, refuses a body that does not validate as `invalid`, and ignores unknown fields | reading the body before the header, or spending the token before validating the body, would burn the operator's token on a request sent to be refused. All three rules *are* checked on `sync`, `events` and `result` |
| Whether the copy of a resent event you keep is the first one | v1 gives a runner no way to read an event back, so nothing outside your hub can see which copy you stored. What is checked: a resent batch is accepted and acknowledged no further back than before |

A real runner is the check for the rest: point `yad connect` at your hub and
run work through it. [`docs/trial.md`](docs/trial.md) walks through that
against `yad hub`; swap in your own URL.

### What the report will not print, and its limit

The report goes to a terminal and into whatever log ran it, so the suite
removes secrets from it: the registration token and credential it was given,
the body of a successful register, and secrets your hub *sends* — a grant
value, a password inside a source URL, credentials in the connection URL.

**Its honest bound, which it states itself:** a hub is free to write anything
into an error message, an error code or a run id, including a secret belonging
to some other run the suite was never offered and cannot know. Nothing in the
suite catches that and nothing in it pretends to. What it guards is every
secret the protocol carries and every secret it has itself held. Your hub's
error messages are yours to keep clean.

## 13. Checklist

Every item links to where it is defined. **(C)** marks what `yad conformance`
checks; the rest is yours to get right.

**Wire**

- [ ] Types generated from `protocol/v1/openapi.yaml`; empty lists omitted, unknown fields ignored — [§2](#2-wire-basics) (C)
- [ ] `426 unsupported_protocol` for a missing or wrong `Yad-Protocol`, before reading the body, on every call — [§3](#3-the-calls) (C on all but register)
- [ ] The error envelope with a `next_action` on every error, unknown paths and wrong methods included — [§10](#10-errors-and-next_action) (C)
- [ ] `internal` only with a `5xx`, and never for a request you will never accept — [§10](#10-errors-and-next_action)
- [ ] A body that does not validate refused as `invalid`, and a body too large with `413` — [§3](#3-the-calls) (C)

**Register and authentication**

- [ ] Register only with a registration token you issued; each token registers one runner once — [§3](#post-runnersregister) (C)
- [ ] A runner credential issued at register authenticates every later call, and nothing else does — [§3](#post-runnersregister) (C)
- [ ] A known runner id re-registers only with a token issued for that runner — [§3](#post-runnersregister)

**Sync**

- [ ] `sync_interval_ms` within 5–60 s and `next_sync_ms` within 3–60 s; `lease_ms` never shorter than either — [§5](#5-leases-timings-and-runners-that-go-away) (C)
- [ ] An offer is claimed only when the next sync lists it; an offer not listed goes back in the queue at that sync — [§3](#post-runnersrunnersync) (C)
- [ ] A listed run the runner does not hold is answered with `cancel` — [§3](#post-runnersrunnersync) (C)
- [ ] Offers within `free_capacity`, both `total` and each `by_harness`, as sent — [§4](#who-may-be-offered-what) (C)
- [ ] Offers only for harnesses that are first-class, present and error-free — [§4](#who-may-be-offered-what)
- [ ] Every offered run passes the rules the schema cannot state — [§4](#rules-the-schema-cannot-state) (C)
- [ ] `start_at` still ahead, `effort`, `fork_from` and `live` sessions only to runners advertising them — `effort` and `fork_from` on the run's harness's own list when the runner advertises `harness_features` — [§7](#7-controls-and-features) (C)
- [ ] A run opening a session with a source on the machine not offered to a runner whose document says `path_sources: false` — [§4](#who-may-be-offered-what)
- [ ] `report_capabilities` when the fingerprint moves without a document — [§3](#post-runnersrunnersync) (C)
- [ ] A sync refused when its body's `runner_id` or its capability document's differs from the path, or its credential is another runner's — [§3](#post-runnersrunnersync) (C)
- [ ] A sync never refused over a dashboard field — load, disk, spool or outbox depth — only over what routing reads — [§3](#post-runnersrunnersync) (C)

**Leases and loss**

- [ ] Every sync renews the lease of every run it lists; lapsed leases are lost, found by a timer as well as by syncs — [§5](#5-leases-timings-and-runners-that-go-away) (C)
- [ ] A claim you asked to cancel ends `cancelled`, not `lost`, when a sync leaves it out or its lease lapses — [§3](#post-runnersrunnersync)
- [ ] An offer lapses with its lease and goes back in the queue; a claim listed after that is answered with `cancel` — [§5](#5-leases-timings-and-runners-that-go-away) (C)
- [ ] `deregister` loses held runs (a claim you cancelled ends `cancelled`), requeues offers, closes the runner's sessions and ends their queued runs, and retires the credential — [§5](#5-leases-timings-and-runners-that-go-away)
- [ ] A runner silent past your abandon-after loses its sessions and their queued runs, keeps its credential, and hears `close_session` when it returns — [§5](#5-leases-timings-and-runners-that-go-away)

**Events and results**

- [ ] Events stored once by `(run, seq)`, `seq` from 1; `acked_through` is the contiguous prefix — [§6](#events) (C)
- [ ] Events only from the claiming runner, before and after the run ends — [§6](#events) (C)
- [ ] Results from the runner the run was offered to or claimed by, applied once; the same state again acknowledged; a different one `409`, a run you ended before any claim included — [§6](#results) (C)
- [ ] A result from the runner a run was offered to taken only while the offer is open; after a sync takes it back or its lease lapses, `403 not_holder` — [§6](#results) (C)
- [ ] A `refused` result before any claim ends the run, and the run is not offered again — [§6](#results) (C)

**Controls, sessions, grants, versions**

- [ ] `cancel` and `interrupt` repeated until the run ends; `steer` sent once; `drain` and `close_session` repeated until answered — [§7](#7-controls-and-features)
- [ ] Login controls only to a runner advertising `login`, each repeated until `logins` answers it; a sync carrying `logins` taken; a token held only until the runner reports its login; a login ended on your word only while never sent; a sent login unheard for thirty minutes ended `failed` and its token blanked; a login reported and then left out ended `failed` — [§7](#7-controls-and-features) (C, as far as the gate and taking the reports)
- [ ] `add` and `remove_account` only to a runner advertising `accounts` to you; `remove_account` repeated until neither the capability document nor health lists the account, or `accounts` is no longer advertised — [§7](#7-controls-and-features) (C, as far as the gate)
- [ ] No gated control to a runner that does not advertise its feature — `steer` and `interrupt` on the list of the run's harness when the runner advertises `harness_features` — [§7](#7-controls-and-features) (C, as far as a runner advertising none of its own and an empty list for its harness)
- [ ] Sessions bound by their first claim, later runs to that runner only, one at a time; `session.new` set right — [§8](#8-sessions)
- [ ] Nothing offered in a session you have sent `close_session` for until its close is reported — [§8](#8-sessions)
- [ ] Closes in `closed_sessions` believed from the holder or the last-offered runner, whatever features it advertises; queued runs in a closed session ended — [§8](#8-sessions)
- [ ] Grant values kept only until the run is terminal — [§9](#9-grants)
- [ ] If you set `min_version`: refused at register before spending the token, and at sync — [§11](#11-versioning)
