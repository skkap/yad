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
neither is a prerequisite.

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
| `POST /runners/{runner}/sync` | every 5–60 s, at the interval you name | the runner's health and the runs it holds in; new runs and instructions out |
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
            any held state ── lease lapses ──► lost   (your decision)
```

`queued` and `offered` are yours; the protocol never carries them. From
`claimed` on, the runner reports the state in every sync until the run ends,
and the terminal state arrives in the result. `lost` is the one terminal
state you decide rather than hear. §4 has the full state machine.

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

- **An absent list is an empty list.** A list that can be empty is omitted,
  not sent as `[]` or `null`. Your encoder must do the same, or a runner that
  round-trips your response will disagree with you about what you offered.
- **Unknown fields are ignored.** Every object allows additional properties,
  so a field added within v1 never breaks an older hub or runner. Do not
  reject what you do not recognise.

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

**Recommended, and what `yad hub` does:** runner ids are not secret, so a
token that could re-register *any* known runner would hand that runner's
sessions, and the grants delivered into them, to whoever held a token. `yad
hub` therefore refuses a known runner id with `409 conflict` unless the token
was issued for that runner (`yad hub token create --runner <id>`); such a
token replaces the runner's credential, and the old one stops working at once.
The runner's sessions stay bound to it.

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
| `health.harnesses` | per harness, `ready` and each account's state. A harness with `ready: false` has every account at a usage limit or needing a login; the runner will not claim for it |
| `health.draining` | the runner has stopped taking work (§7) |
| `runs` | **every run the runner holds**, with its state. Listing is claiming and lease renewal (below) |
| `closed_sessions` | sessions the runner has closed, repeated until answered (§8) |

The rest of `health` — load, disk, spool and outbox depth, recent errors — is
for your operators and dashboards. It is the runner's own words, bounded, and
never carries a credential.

**What you must do, in this order** (the order is `yad hub`'s, and each step
says why it goes where it does):

1. **Settle what time has decided** (§5): offers whose lease lapsed go back
   in the queue, held runs whose lease lapsed are lost, and runners silent past
   your abandon-after lose their sessions — so a runner back from a long
   absence hears what became of its runs rather than renewing them.
2. **Take the document** if the request carries one: it replaces the one you
   hold. If it carries none and the fingerprint differs from the one you hold,
   answer with a `report_capabilities` control, and until the document
   arrives, send no control gated on a feature and offer no run that needs one
   (§7): what the runner acts on is unknown.
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
     its cancel — the runner would have to guess which one you meant.
   - then add the controls you have for each held run (§7).
5. **Take the closes** in `closed_sessions` (§8), and add a `close_session`
   control for each session you are closing on this runner.
6. **Take back every run you offered this runner that this sync did not
   list.** It was never received. It goes back in your queue, for this runner
   or another.
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

**Response:** `next_sync_ms` and `lease_ms` (required), `runs` (the offers),
`controls`, `min_version`. Omit `runs` and `controls` when empty.

**Refusals** (`yad hub`): `400 invalid` for a body that does not validate or a
`runner_id` differing from the path; `401 unauthorized` for no credential or
one you do not know; `403 unauthorized` for a credential belonging to another
runner; `426 version_too_old` under your floor. A runner stops syncing your
hub on `unauthorized`, `runner_revoked`, `version_too_old` and
`unsupported_protocol` until its owner acts; on anything else it backs off —
1 s doubling to 30 s — and tries again (§10).

### `POST /runs/{run}/events`

**Authenticated by the runner credential.** The path names the run; the
credential alone says which runner is calling.

**Request:** `{"events": [...]}`, events of this one run in `seq` order. A yad
runner sends a batch about every second, or sooner at 100 events, from a
durable spool, so a network failure loses nothing and a batch may repeat
events you already hold.

**What you must do:** accept events only from the runner the run was claimed
by — before and after it ends — store each one once by `(run, seq)`, and
answer how far the run's stream is now complete. §6 has the rules.

**Response:** `{"acked_through": N}` — the highest `seq` up to which you hold
every event of this run with no gap.

**Refusals** (`yad hub`): `400 invalid` for a `seq` below 1 or more than 1000
events in one batch; `401 unauthorized`; `403 not_holder` for a run this
runner does not hold; `404 not_found` for a run you have never heard of;
`413` for a body over 16 MiB — the runner halves the batch and tries again.

### `POST /runs/{run}/result`

**Authenticated by the runner credential.**

**Request:** the run's terminal report — `state` (one of the five terminal
states), `final_text`, `error` with a class and message, `usage` by model,
`metrics`, and `last_seq`, the seq of the run's last event. The runner writes
it durably before the first attempt and retries until you answer 2xx.

**What you must do:** accept it from the runner the run was offered to *or*
claimed by; apply it at most once; acknowledge the same state again; refuse a
different one with `409 conflict`. §6 has the rules, including why the door
is wider than for events.

**Response:** `{"ok": true}`.

**Refusals** (`yad hub`): `400 invalid` for a state that is not terminal;
`401 unauthorized`; `403 not_holder`; `404 not_found`; `409 conflict` when you
already hold a different terminal state; `413` for a body over 16 MiB.

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

A current yad runner does not call `deregister` yet: `yad disconnect`, which
will, is still being designed. Implement it anyway; the conformance suite
lists it among the rules it cannot check (§12).

## 4. Runs

### What a run carries

A run is one turn of one harness against one session: one instruction in,
one terminal state out. It names its `session`, `harness` and `model`
explicitly — a runner infers none of them — and carries a `brief` (the
`instruction`, and optional `context` appended to the harness's system
prompt), optional `sources` to build the session's working directory from,
optional `grants` (§9), and optional timings: `start_at`, `max_wait_ms`,
`wall_clock_ms` and `inactivity_ms`. The document describes each field and its
absent case.

There is deliberately no permission, sandbox or tool-policy field. A hub
cannot set or widen what a harness may do on someone's machine.

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
  `ANTHROPIC_BASE_URL`, `ANTHROPIC_CUSTOM_HEADERS`, anything beginning
  `CLAUDE_CODE_USE_`, `CODEX_HOME`, `OPENAI_API_KEY`, `CODEX_API_KEY`,
  `CODEX_ACCESS_TOKEN`, `OPENAI_BASE_URL`,
  `CODEX_REFRESH_TOKEN_URL_OVERRIDE` and `AWS_BEARER_TOKEN_BEDROCK`, whatever the run's harness. If the project itself needs
  one of these keys — to run its tests, say — send it under another name and
  have the brief say which. `protocol/v1/grant.go` is the list itself; this is a
  copy of it.
- every name above is matched whatever its case.
- no two grants share a name, and no two **file** grants have names differing
  only by case — on a case-folding filesystem they become one file holding
  silently the second value.

A run breaking any of them is refused **whole**, not stripped of the offending
part.

**A run id is used once.** A runner never runs the same run id twice: offered
a run id it already ran — say, one it reported `lost` after a restart — it
refuses it. To try work again, make a new run.

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
in the future goes only to a runner advertising `start_at`; a run whose
`session.mode` is `live` only to one advertising `live_sessions` (none does
yet). §7 has the table.

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
   any non-terminal state ── lease lapses ──► lost      (decided by the hub)
```

| state | whose | meaning |
|---|---|---|
| `queued` | hub | waiting for a runner that can take it |
| `offered` | hub | sent in a sync response; not yet listed back. It goes back to `queued` when the next sync leaves it out, or when `lease_ms` passes with no sync claiming it |
| `claimed` | runner | listed in a sync; not yet started |
| `preparing` | runner | building the workdir, running the repository's setup hook, choosing an account |
| `running` | runner | the harness is running. A finished run whose result you have not yet acknowledged stays listed as `running`, so its lease outlasts an outage on your side |
| `waiting` | runner | parked on a usage limit with no process; `resumes_at` says when it expects to continue. It survives a runner restart |
| `succeeded`, `failed`, `cancelled`, `timed_out` | runner | terminal, reported in the result |
| `lost` | hub, usually | terminal. You record it when a lease lapses or the runner deregisters. A runner also *reports* `lost` for a run a previous process of it was holding when it died |

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
| `refused` | failed | the runner would not take the run — invalid, a harness it cannot drive, a session rule broken. Retrying the same run changes nothing; the message says why |
| `session_closed` | failed | the run names a session the runner has closed or is closing. Start a new session |
| `resume_rejected` | failed | the harness had no conversation to continue: the transcript is gone. Start a new session |
| `session_mismatch` | failed | the harness ran under another session id; the conversation's context is lost |
| `grants_lost` | lost | a run parked on a usage limit was picked up by a later runner process, and its grants did not survive. Submit a new run, with its grants |
| `max_wait_exceeded` | timed_out | it waited longer than `max_wait_ms` for a free account |
| `wall_clock_timeout`, `inactivity_timeout` | timed_out | stopped by the run's own caps |
| `source_refused` | failed | a source breaks the owner's rules — a path outside the allowed directories, a transport the runner does not use |
| `source_failed`, `setup_failed`, `prepare_failed` | failed | the workdir could not be built, or the repository's setup hook failed |
| `prompt_too_long` | failed | the conversation no longer fits the model's context |
| `runner_stopping` | cancelled | the runner cancelled it on its way down, not you |
| `runner_restarted` | lost | the runner process holding it stopped without finishing it |
| `harness_error`, `harness_exited`, `harness_start_failed`, `adapter_error` | failed | the harness failed, or could not be started |

`steer_failed` and `interrupt_failed` appear only as `error` *events*, never
in a result: the control did not reach the harness, and the run carried on.

## 5. Leases, timings, and runners that go away

**The interval you name must be between 5 s and 60 s.** `sync_interval_ms` at
register and `next_sync_ms` in every sync response are both bounded,
inclusive. A hub naming 2 s or 5 min is refused by conformance and clamped by
a runner. `yad hub` uses 15 s. The runner adds ±10 % jitter, syncs at once
when a response carried offers, and backs off from 1 s to 30 s after a failed
sync.

**Leases.** Every sync renews the lease on every run it lists. A run whose
lease lapses is **lost**: the runner is gone or has stopped talking, and the
run will not be reported. Decide what lost means for you, but decide it; a run
in a non-terminal state that nothing will ever end is a queue that only grows.
Lapsed leases must be found even when nothing syncs — `yad hub` sweeps on a
timer at the sync interval, as well as at the start of every sync — because a
hub whose only runner went away gets no syncs to notice it by.

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
the runs it holds become `lost`; the runs offered to it and not yet claimed go
back in the queue; and **every session bound to it closes, with the runs still
queued in those sessions ending** — `yad hub` fails them with a reason that
says to submit the work to a new session. **Do not unbind the sessions
instead.** A session is resumable only on the runner that holds it, because its
transcript is on that runner's disk; offering it to another runner offers a
resume that cannot work. And do not leave them open and bound: a queued run
holds no lease, so nothing would ever end it, and it waits for good on a
runner that has gone. Then retire the credential — `yad hub` keeps the
runner's row, so a token issued for that runner id brings it back.

**A runner that stops syncing without deregistering is settled by time, in
three steps** ([0046](docs/decisions/0046-a-silent-runner-loses-its-offers-with-the-lease-and-its-sessions-after-a-day.md)).
It crashed, was switched off, or lost its network; it may come back.

- **Its held runs are lost when their leases lapse**, as above.
- **An offer to it lapses with the same lease.** The `lease_ms` beside an offer
  covers the offer: one the runner has not claimed within it goes back in your
  queue, and you may offer it to any runner. A claim that arrives after that —
  the runner back, listing the run — is answered with a `cancel`, exactly as
  for a run whose lease lapsed after it was claimed: by then the run may be
  another runner's. `yad hub` also leaves that run out of the offers in the
  answer carrying the cancel, so no runner is told to stop and start the same
  run at once. Conformance checks the lapse and the cancel.
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
  still leased to it. `yad hub` uses a day (`yad hub serve --abandon-after`),
  and does not count time it was itself down. Conformance cannot wait out a
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
| `tool_result` | `tool.id` (joining it to its call), `tool.output`, `tool.truncated` |
| `status` | `status`, a short label for a phase of the run, and sometimes `text`. For people: not a closed set, and not to be parsed |
| `usage` | `usage`: tokens one model used |
| `error` | `error`: a class and message, whether or not the run carries on |

Tool input and output are capped at 8 KiB each, and `truncated` says when
either was cut; text, error messages and a result's final text at 1 MiB each.
These bind the runner, not you, but they tell you what to size storage for.

**Events are idempotent by `(run, seq)`, and sequences start at 1.** A resent
event is not a new one, and the first copy you stored stands. **`seq` is
one-based** — an empty stream is acknowledged through `0`, and a zero-based
implementation silently never acknowledges anything, because it treats the
runner's first event as filling a gap that does not exist.

Answer with `acked_through`: the highest `seq` below which you hold every event
**with no gap**. It is authoritative — the runner resends everything after it —
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
tells you not to try again.

**Results are idempotent, and applied at most once.** The same terminal state
again is acknowledged. A *different* terminal state is `409`, and yours
stands: a runner reporting `succeeded` for a run you already recorded `lost`
is told so and stops.

A result from any other runner is refused, `403 not_holder` or `404
not_found`, and never applied.

### How a runner treats your answers to events and results

This is what makes the statuses matter:

| your answer | events batch | result |
|---|---|---|
| 2xx | the spool is trimmed through `acked_through` | done |
| `409 conflict` | retried | done: your state stands |
| a 4xx with code `not_holder` or `invalid` | dropped: they stay only in the runner's local spool | dropped |
| `413` | sent again in halves, down to one event at a time | retried |
| anything else — `401`, `404`, a `5xx`, a proxy's bare 4xx | retried | retried, backing off to once every 5 minutes, and replayed at every runner start |

So a `404` is never final — it may be a wrong connection URL rather than an
unknown run — and a refusal you mean to be final must say `not_holder` or
`invalid`.

## 7. Controls and features

**Controls are not acknowledged.** Nothing in the protocol tells you a control
arrived — the only evidence is what the runner does next. So each control has
a rule for when to stop sending it:

| control | carries | what the runner does | send it |
|---|---|---|---|
| `cancel` | `run_id` | ends the run: interrupts the harness, then signals its process group, then kills it. A waiting run ends where it stands. A run whose claim you answer with a cancel never starts, and no result is owed | in every response to a sync that lists the run, until the run ends. The runner acts on the first |
| `interrupt` | `run_id` | ends the current turn and keeps the session; the run ends `cancelled` unless the turn finished first. An interrupt that reaches a run before its harness is up ends it as a cancel does | the same as `cancel` |
| `steer` | `run_id`, `text` | hands the text to the running harness as more input, at its next tool boundary or after the turn. One the harness would not take appears as an `error` event with class `steer_failed` | **once**. Repeat it and the harness reads the text twice |
| `close_session` | `session_id` | closes the session and deletes its workdir; a session with a run held closes when that run ends; one it does not hold or already closed is reported closed | in every response until the session appears in the runner's `closed_sessions` (§8) |
| `drain` | nothing | stops claiming, lets the runs it holds finish, then exits. Its health says `draining` and its free capacity is zero | in every response until a sync's health says `draining` |
| `report_capabilities` | nothing | sends its capability document in the next sync | while the fingerprint differs from the document you hold |
| `update` | — | reserved; never send it | never |

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
| `close_session` | the `close_session` control, and `closed_sessions` in sync |
| `start_at` | a run carrying `start_at` — offer one only to a runner that advertises it |
| `live_sessions` | a run whose `session.mode` is `live` — offer one only to a runner that advertises it. The spec lists `live` as an enum value and connects it to no feature, so this pairing exists only here |

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

**A draining runner is offered nothing.** Stop offering to a runner as soon as
you have decided to drain it, not only once its health says `draining` — an
offer made in between would only come back.

## 8. Sessions

A session is a durable conversation with one harness in one working directory,
on one runner. You choose its id; the runner maps it to the harness's own
session id, so you never need to know what a transcript file is.

**`session.new` is `true` for the run that opens a session and `false` for
every later run.** A runner refuses `new: true` for a session id it already
has, and `new: false` for one it does not hold, rather than guess: guessing
would resume a conversation that does not exist, or silently start over one
that does. A later run names the same `sources` as the first, or none.

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

- record closes by session id, and take a repeat as the same news;
- stop offering runs in a closed session: the runner refuses them with class
  `session_closed`;
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
and a session is closed only by the one whose disk it would be on.

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
| `invalid` | a body that does not validate, or a wrong method | `400`, or `405` for a method | on events or a result: drops it. On sync: backs off and retries |
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
`protocol_features` strings are ignored. A new control kind is sent only to a
runner that advertises a feature for it, so an older runner never sees one.

**Features, both ways.** The runner advertises `protocol_features` in its
capability document; the hub may advertise `hub_features` at register. Nothing
is used that the other side did not advertise (§7). v1 defines no hub
features. A runner whose fingerprint moved without the document it promised is
treated as advertising nothing until the document arrives.

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

Thirty-seven black-box checks against a URL, written from the protocol rather
than from `yad hub`'s internals — nothing in the suite imports the hub, so it
tests the protocol and not one implementation of it. A failure gives you the
rule as a sentence and the part of `ARCHITECTURE.md §2` it is filed under;
each part of §2 points to the section of this page with the rule.

The URL is your connection URL — every path is relative to it, so a hub
mounted at `https://example.com/yad/v1` is named in full. Each token is a
registration token your hub issued for a **new** runner and nobody has used;
`-` reads one from standard input, and only one of the two can be `-`. The
command exits 0 when nothing failed and 1 otherwise.

**What it does to your hub.** It spends the registration token you give it and
registers a runner advertising a harness no real run asks for —
`yad-conformance`, or `--harness` — so a suite pointed at a live hub is offered
nothing anyone was waiting on.

**Queue at least three runs for that harness first, and be able to offer two
at once.** The lease rules need two runs held simultaneously — one to report on,
one to leave unrenewed until its lease lapses — so one run is not enough, and
**two offered one at a time is also not enough**. Offering one run per sync
breaks no rule in §2 and is a perfectly good hub; it simply leaves those checks
unreachable, and the suite says so rather than failing you. The third is left
offered and unclaimed until its offer's lease lapses. More than three does no
harm. The suite reports one run `failed` with class `refused`, leaves one held
run to lose its lease and the offered one to lapse; `--lease-wait` (default
90 s, `0` skips those rules) bounds how long it waits, and must outlast the
`lease_ms` you name.

Without them those checks are **skipped**, and each skip says what to queue to
make it possible.

**Give it a second registration token to check who holds a run.**
`--second-token` spends it on a second runner, which sends a batch of events
and a result for the run the first runner holds. Both must be refused with
`403 not_holder`, the code a runner stops on, or `404 not_found` if your hub
will not name a run to a runner that does not hold it; any other refusal fails,
because a `5xx` or a `401` sends the runner back to retry what you will never
take. Without the second token those two checks are skipped, saying which flag
would make them possible;
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

It ends `37 passed, 0 failed, 0 skipped`, after about a minute spent waiting
out a lease.

### What it does not check

The suite prints this list itself, and it is nine rules — not a footnote. Each
is something **your hub still has to get right** with nothing to catch you:

| rule | why the suite cannot reach it |
|---|---|
| `POST /runners/{runner}/deregister` — held runs lost, offers requeued, the runner's sessions closed and their queued runs ended | deregistering retires the runner every other check is made as; the second runner `--second-token` registers could carry it, and does not yet. `yad hub` implements it; implement it in yours |
| The controls — `cancel`, `interrupt`, `steer`, `close_session`, `drain` — their repetition until the runner acts, and a `steer` being delivered once | nothing in v1 lets a *runner* ask for a control, so the suite can only wait for one it cannot cause |
| `start_at`, `min_version`, and the feature gates on `drain`, `steer`, `interrupt`, `close_session`, `start_at` | each needs a run or control the protocol gives a runner no way to request. The other half *is* checked: that a hub sends no control it should have gated |
| Sessions staying put — first claim binds the session to that runner, later runs to that runner alone, one at a time | needs two runs in one session, which only your own queueing can arrange |
| A runner silent past your abandon-after — its sessions closed and their queued runs ended, its credential kept, `close_session` sent when it returns | the silence is a day by default and every hub names its own, which v1 gives a runner no way to ask |
| Capacity shared round the hubs, one pool, a unit at a time | a rule about one runner across several hubs, so no single hub can pass or fail it |
| The caps on tool output, event text and final text, and halving a batch a proxy refused | those bind the runner, not the hub |
| Whether `register` refuses a missing or wrong `Yad-Protocol`, and ignores unknown fields | reading the body before the header would burn the operator's token on a header check. Both rules *are* checked on `sync`, `events` and `result` |
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

**Register and authentication**

- [ ] Register only with a registration token you issued; each token registers one runner once — [§3](#post-runnersregister) (C)
- [ ] A runner credential issued at register authenticates every later call, and nothing else does — [§3](#post-runnersregister) (C)
- [ ] A known runner id re-registers only with a token issued for that runner — [§3](#post-runnersregister)

**Sync**

- [ ] `sync_interval_ms` and `next_sync_ms` within 5–60 s; `lease_ms` never shorter than either — [§5](#5-leases-timings-and-runners-that-go-away) (C)
- [ ] An offer is claimed only when the next sync lists it; an offer not listed goes back in the queue — [§3](#post-runnersrunnersync) (C)
- [ ] A listed run the runner does not hold is answered with `cancel` — [§3](#post-runnersrunnersync) (C)
- [ ] Offers within `free_capacity`, both `total` and each `by_harness`, as sent — [§4](#who-may-be-offered-what) (C)
- [ ] Offers only for harnesses that are first-class, present and error-free — [§4](#who-may-be-offered-what)
- [ ] Every offered run passes the rules the schema cannot state — [§4](#rules-the-schema-cannot-state) (C)
- [ ] `start_at` still ahead and `live` sessions only to runners advertising them — [§7](#7-controls-and-features) (C)
- [ ] `report_capabilities` when the fingerprint moves without a document — [§3](#post-runnersrunnersync)

**Leases and loss**

- [ ] Every sync renews the lease of every run it lists; lapsed leases are lost, found by a timer as well as by syncs — [§5](#5-leases-timings-and-runners-that-go-away) (C)
- [ ] An offer lapses with its lease and goes back in the queue; a claim listed after that is answered with `cancel` — [§5](#5-leases-timings-and-runners-that-go-away) (C)
- [ ] `deregister` loses held runs, requeues offers, closes the runner's sessions and ends their queued runs, and retires the credential — [§5](#5-leases-timings-and-runners-that-go-away)
- [ ] A runner silent past your abandon-after loses its sessions and their queued runs, keeps its credential, and hears `close_session` when it returns — [§5](#5-leases-timings-and-runners-that-go-away)

**Events and results**

- [ ] Events stored once by `(run, seq)`, `seq` from 1; `acked_through` is the contiguous prefix — [§6](#events) (C)
- [ ] Events only from the claiming runner, before and after the run ends — [§6](#events) (C)
- [ ] Results from the runner the run was offered to or claimed by, applied once; the same state again acknowledged; a different one `409` — [§6](#results) (C)
- [ ] A `refused` result before any claim ends the run — [§6](#results)

**Controls, sessions, grants, versions**

- [ ] `cancel` and `interrupt` repeated until the run ends; `steer` sent once; `drain` and `close_session` repeated until answered — [§7](#7-controls-and-features)
- [ ] No gated control to a runner that does not advertise its feature — [§7](#7-controls-and-features) (C, as far as a runner advertising none)
- [ ] Sessions bound by their first claim, later runs to that runner only, one at a time; `session.new` set right — [§8](#8-sessions)
- [ ] Closes in `closed_sessions` believed from the holder or the last-offered runner; queued runs in a closed session ended — [§8](#8-sessions)
- [ ] Grant values kept only until the run is terminal — [§9](#9-grants)
- [ ] If you set `min_version`: refused at register before spending the token, and at sync — [§11](#11-versioning)
