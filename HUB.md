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
yad conformance <your-hub-url>
```

**This section is not yet accurate and is marked so deliberately.** The
conformance suite is DEV-33 and is not merged as of this draft;
`internal/conformance` holds only its package doc. When it lands this section
must say, per behaviour above, whether the suite checks it — and must name the
ones it cannot reach rather than leaving a reader to assume coverage it does
not have. §2 rules that no automated check can reach include `deregister`,
which ends the credential the suite would need to keep testing with.

Until then, treat every behaviour above as unchecked by the suite.
