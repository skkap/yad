# Hosting the hub half of the v1 protocol

A runner speaks to hubs. This is what a hub has to do to be one of them.

It is generic on purpose: how a particular product queues work, authenticates
people, or stores runs is that product's own business. What follows is only
the part a runner can tell the difference about.

**[`ARCHITECTURE.md §2`](ARCHITECTURE.md#2-the-protocol--v1) is the protocol.**
This document does not restate it, and where the two disagree §2 is right —
two copies of a wire description drift, and a reader cannot tell which is
current. §2 gives the calls, the field-by-field shapes and the rules. This
gives the things you would get wrong anyway: what the generated types cannot
say, what a hub must *do* rather than accept, and what is checked.

## Start from the document, not from this page

`protocol/v1/openapi.yaml` is generated from the Go types and committed. It is
the contract.

```
npx openapi-typescript protocol/v1/openapi.yaml -o src/protocol.ts
```

Everything a runner sends and expects is in there. Two wire rules that the
types express but people override anyway, both from §2:

- **An absent list is an empty list.** A list that can be empty is omitted, not
  sent as `[]` or `null`. Your encoder must do the same, or a runner that
  round-trips your response will disagree with you about what you offered.
- **Unknown fields are ignored.** Every object allows additional properties, so
  a field added within v1 never breaks an older hub or runner. Do not reject
  what you do not recognise.

### What the types cannot tell you

**A `Source` is exactly one of `git` or `path`.** The document says this with
a `oneOf`, so generated types give you a union and TypeScript will keep you
honest. `yad hub` enforces the same rule in code rather than from the schema,
so a run naming both comes back `400 invalid` with
`sources[0]: set git or path, not both` rather than a schema-shaped
complaint. Enforce it however you like; just do not accept both, and do not
accept neither.

## What a hub must do

The calls are in §2. These are the behaviours behind them — the places where
accepting the right JSON is not enough.

**Claim by listing.** A run you offer in a sync response is *not* claimed by
that response. It is claimed when the runner's **next** sync lists it. A run
you offered and the next sync does not list was never received, and is yours
to offer again — to this runner or another. A hub that treats the offer as the
claim loses every run a runner never got.

**Never offer more than the free capacity.** The sync request's
`health.free_capacity` is what the runner has already reserved for you. Offer
more and the surplus is refused, having occupied your queue in the meantime.

**Leases.** Every sync renews the lease on every run it lists. A run whose
lease lapses — `yad hub` uses four missed intervals — is **lost**: the runner
is gone or has stopped talking, and the run will not be reported. Decide what
lost means for you, but decide it; a run in a non-terminal state that nothing
will ever end is a queue that only grows.

**Never name a `lease_ms` shorter than the `next_sync_ms` beside it.** This is
the one timing rule you cannot derive from the field names, and getting it
wrong punishes the runners behaving best: a lease shorter than the interval
lapses on a runner that synced *exactly* when you asked it to, and you take
back the runs of a runner doing everything right. Four intervals is the
default and one interval is the floor. Fewer than four is fine; fewer than one
is the bug.

**Events are idempotent by `(run, seq)`.** A resent event is not a new one.
Answer with `acked_through`: the highest `seq` below which you hold every
event with no gap. It is authoritative — the runner resends everything after
it — so returning a number you have not actually stored contiguously loses
events silently.

**Results are idempotent, and applied at most once.** The same terminal state
again is acknowledged. A *different* terminal state is `409`, and yours
stands: a runner reporting `succeeded` for a run you already recorded `lost`
is told so and stops.

**Controls are not acknowledged.** Nothing in the protocol tells you a control
arrived. So `cancel` and `interrupt` are repeated in every response to a sync
that still lists the run, until the run ends. A `steer` is sent **once** —
repeat it and the harness reads the text twice. `drain` repeats until the
runner's health says `draining`; `close_session` repeats until the session
appears in `closed_sessions`.

**Every error is the envelope, and `next_action` is mandatory.**

```json
{"error": {"code": "invalid", "message": "...", "next_action": "..."}}
```

`code` is one of `not_implemented`, `unauthorized`, `runner_revoked`,
`version_too_old`, `conflict`, `not_found`, `invalid`,
`unsupported_protocol`, `not_holder`. Every response under your base path
carries it — an unknown path and a wrong method included. A plain-text 404 is
a protocol violation, because the runner on the other end is debugged by
someone reading `next_action`.

**Refuse the wrong protocol before reading the body.** A request whose
`Yad-Protocol` header is missing or names another version is `426
unsupported_protocol`.

## Features are promises, not decoration

A runner advertises `protocol_features` in its capability document. They exist
so a hub does not send a control that would be silently ignored — nothing
acknowledges a control, so an ignored one is indistinguishable from an obeyed
one to whoever asked for it.

| feature | what it gates |
|---|---|
| `steer` | the `steer` control |
| `interrupt` | the `interrupt` control |
| `drain` | the `drain` control |
| `close_session` | the `close_session` control, and `closed_sessions` in sync |
| `start_at` | a run carrying `start_at` — offer one only to a runner that advertises it |

Send a gated control to a runner that does not advertise its feature and
nothing happens, for ever. `yad hub` answers such a request `409` rather than
queueing it.

## Proving it

```
yad conformance <hub url> --token <token> [--harness id] [--lease-wait d]
```

Thirty-four black-box checks against a URL, written from §2 rather than from
`yad hub`'s internals — nothing in the suite imports the hub, so it tests the
protocol and not one implementation of it. A failure gives you the rule as a
sentence and the part of §2 it is written in, because whoever reads it is
implementing a hub and does not have this repository open.

**What it does to your hub.** It spends the registration token you give it and
registers a runner advertising a harness no real run asks for, so a suite
pointed at a live hub is offered nothing anyone was waiting on. The rules about
claiming, events, results and the lease need a real run: queue one or two for
that harness first. Without them those checks are **skipped**, and each skip
says what to queue to make it possible.

**A skip is not a pass.** It is the suite saying your hub gave it no way to
ask. Read the skips before believing the passes.

### What it does not check

The suite prints this list itself, and it is nine rules — not a footnote. Each
is something **your hub still has to get right** with nothing to catch you:

| rule | why the suite cannot reach it |
|---|---|
| `POST /runners/{runner}/deregister` | `yad hub` answers 501 there while the behaviour is built (DEV-81). Implement it in yours. |
| The controls — `cancel`, `interrupt`, `steer`, `close_session`, `drain` — their repetition until the runner acts, and a `steer` being delivered once | nothing in v1 lets a *runner* ask for a control, so the suite can only wait for one it cannot cause |
| `start_at`, `min_version`, and the feature gates on `drain`, `steer`, `interrupt`, `close_session`, `start_at` | each needs a run or control the protocol gives a runner no way to request. The other half *is* checked: that a hub sends no control it should have gated |
| Sessions staying put — first claim binds the session to that runner, later runs to that runner alone, one at a time | needs two runs in one session, which only your own queueing can arrange |
| Capacity shared round the hubs, one pool, a unit at a time | a rule about one runner across several hubs, so no single hub can pass or fail it |
| The caps on tool output, event text and final text, and halving a batch a proxy refused | those bind the runner, not the hub |
| Whether events and results are refused from a runner that is not the holder *while another runner holds it* | needs two runners and so two registration tokens; the suite holds one. The near half is checked: a run it cannot match to the caller is refused |
| Whether `register` refuses a missing or wrong `Yad-Protocol`, and ignores unknown fields | reading the body before the header would burn the operator's token on a header check. Both rules *are* checked on `sync`, `events` and `result` |
| Whether the copy of a resent event you keep is the first one | v1 gives a runner no way to read an event back, so nothing outside your hub can see which copy you stored. What is checked: a resent batch is accepted and acknowledged no further back than before |

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
