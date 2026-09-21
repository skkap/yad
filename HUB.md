# Hosting the hub half of the v1 protocol

A runner speaks to hubs. This is what a hub has to do to be one of them.

It is generic on purpose: how a particular product queues work, authenticates
people, or stores runs is that product's own business. What follows is only
the part a runner can tell the difference about.

**The shapes are in `protocol/v1/openapi.yaml`; the rules are here.** Every
behaviour a hub can fail `yad conformance` for not knowing is written below —
you should not need anything else to build one. Field-by-field definitions
live in the spec, which your generator reads, and
[`ARCHITECTURE.md §2`](ARCHITECTURE.md#2-the-protocol--v1) is their authority
and settles any disagreement: two copies of a wire description drift, and a
reader cannot tell which is current. But §2 is not a prerequisite for this
page. If you find yourself needing it to answer a question about *behaviour*,
that is a gap here and worth reporting.

## Start from the document, not from this page

`protocol/v1/openapi.yaml` is generated from the Go types and committed. It is
the contract.

```
npx openapi-typescript protocol/v1/openapi.yaml -o src/protocol.ts
```

Every *body* a runner sends and expects is in there, and so are the two headers
a hub checks: `Yad-Protocol` is a required header parameter on each operation,
and the bearer is the `runner` security scheme. Two wire rules the types do express, and which
hand-written encoders override anyway:

- **An absent list is an empty list.** A list that can be empty is omitted, not
  sent as `[]` or `null`. Your encoder must do the same, or a runner that
  round-trips your response will disagree with you about what you offered.
- **Unknown fields are ignored.** Every object allows additional properties, so
  a field added within v1 never breaks an older hub or runner. Do not reject
  what you do not recognise.

### What the types cannot tell you

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

**`Yad-Protocol: 1` is on every request.** The value is the protocol's major
version, `1`, and that is the only value a v1 hub accepts. `openapi.yaml`
declares it on every operation as a required header whose only value is `"1"`,
beside the `426` that refusing it produces. A request missing it or naming
another version is refused before its body is read — see below.

## Authenticating a runner

**`register` is authenticated by a registration token your hub issued**, sent
as `Authorization: Bearer <token>` — the spec says `type: http, scheme:
bearer` and nothing spells the header out. Register no runner without one, and
none with a bearer you did not issue.

**A registration token registers one runner once.** Presented a second time,
for any runner, it is refused. It is spent whether or not the runner it
registered still exists.

That is the whole of the rule: a token that *registered* a runner is spent. A
registration you *refused* — a body that does not validate, a runner id that
conflicts — need not spend it, and the protocol does not say either way. `yad
hub` does not, so an operator who fixes the request can retry with the same
token.

**`register` exchanges that token for a runner credential**, and that
credential authenticates **every later call**, in the same
`Authorization: Bearer` form. A sync, an events batch or a result carrying no
bearer, or one you did not issue, is refused.

The register answer also carries `sync_interval_ms` and `lease_ms`, which are
governed by the timing rules below. Its `hub_features` and `min_version` are
optional and no rule in §2 constrains them: `hub_features` is yours to name if
you want runners to know something about you, and `min_version` refuses
runners older than a version you choose, with `version_too_old`. **Omitting
both is a complete, conforming hub** — nothing checks either, and a hub that
never refuses on version never emits that code.

## What a hub must do

The calls are in §2. These are the behaviours behind them — the places where
accepting the right JSON is not enough.

**Claim by listing.** A run you offer in a sync response is *not* claimed by
that response. It is claimed when the runner's **next** sync lists it. A run
you offered and the next sync does not list was never received, and is yours
to offer again — to this runner or another. A hub that treats the offer as the
claim loses every run a runner never got.

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
surplus is refused, having occupied your queue in the meantime.

**A run the runner does not hold, but which its sync listed, is answered with a
`cancel` for that run.** This is the other direction of claim-by-listing and it
is a separate obligation: the run may have been offered to another runner,
already finished, or never offered at all. Silence leaves the runner holding
something you have no record of.

**Leases.** Every sync renews the lease on every run it lists. A run whose
lease lapses — `yad hub` uses four missed intervals — is **lost**: the runner
is gone or has stopped talking, and the run will not be reported. Decide what
lost means for you, but decide it; a run in a non-terminal state that nothing
will ever end is a queue that only grows.

**The interval you name must be between 5 s and 60 s.** `sync_interval_ms` at
register and `next_sync_ms` in every sync response are both bounded, inclusive.
A hub naming 2 s or 5 min is refused by conformance and clamped by a runner.

**Never name a `lease_ms` shorter than the interval beside it — in *either*
response.** Register answers with `lease_ms` next to `sync_interval_ms`, and
every sync answers with `lease_ms` next to `next_sync_ms`; both pairs are
checked, and a hub that gets the sync right and the registration wrong fails.
Getting it wrong punishes the runners behaving best: a lease shorter than the
interval lapses on a runner that synced *exactly* when you asked it to, and you
take back the runs of a runner doing everything right. Four intervals is the
default and one interval is the floor. Fewer than four is fine; fewer than one
is the bug.

**Events are idempotent by `(run, seq)`, and sequences start at 1.** A resent
event is not a new one. **`seq` is one-based** — an empty stream is
acknowledged through `0`, and a zero-based implementation silently never
acknowledges anything, because it treats the runner's first event as filling a
gap that does not exist.

Answer with `acked_through`: the highest `seq` below which you hold every event
**with no gap**. It is authoritative — the runner resends everything after it —
so returning a number you have not actually stored contiguously loses events
silently. A batch leaving a gap does not move it past the gap; the batch that
fills the gap moves it over everything already held.

**Only the runner a run was claimed by may append events to it.** Events for a
run you cannot match to the calling runner are refused: `403 not_holder`, or
`404 not_found` for a run you have never heard of.

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
and a session is closed only by the one whose disk it would be on. Answer the
sync normally either way; the report is repeated until a sync carrying it is
answered, not until it is believed.

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

**One case the protocol does not yet specify: a runner that stops syncing
without deregistering.** You will meet it, and nothing here or in the spec
tells you what to do, so decide deliberately rather than by accident. Its held
runs are covered — their leases lapse and they are lost — but two things are
not:

- **An offer to it.** Re-offering is defined by the runner's *next* sync
  leaving the run out. If there is no next sync — the runner crashed, or was
  switched off — nothing says when you may take the offer back.
- **Its sessions.** Later runs in a session bound to it go to it alone, and
  nothing says when you may decide it has gone and close them as deregister
  would. Until you do, every later run in such a session waits.

**Keep accepting events after the run has ended.** A batch still in the
runner's spool when the result landed is not late, it is owed — reject it and
the record of the run is permanently short of what happened in it.

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

**Every error is the envelope, and `next_action` is mandatory.**

```json
{"error": {"code": "invalid", "message": "...", "next_action": "..."}}
```

v1 names ten codes: `not_implemented`, `unauthorized`, `runner_revoked`,
`version_too_old`, `conflict`, `not_found`, `invalid`,
`unsupported_protocol`, `not_holder`, `internal`. They are not a closed set —
the spec types `code` as a plain string — so a runner meeting one it does not
know goes by the status and shows `message` and `next_action` to a person. A
wrong method takes `invalid`, which is what `yad hub` sends.

**`internal` is a fault on your side**, sent with a `5xx`: the request may have
been fine and the same one later may succeed. A runner treats it as any `5xx`
— the sync loop backs off and tries again, and events and a result stay
spooled and are sent again until you take them — so it never stops a runner
and never loses a report. That is exactly why it must not stand in for a
refusal: a request you will never accept, sent back as `internal`, is retried
for ever. Send the status that is true and a `next_action` a person can act on;
that pair is what a runner's operator reads.

Every error response under your base path carries this envelope — an unknown
path and a wrong method included. A plain-text 404 is a protocol violation,
because the runner on the other end is debugged by someone reading
`next_action`.

**The envelope is required; most of the status numbers are yours.** The
protocol fixes only a few: `426` for a missing or wrong `Yad-Protocol`, `409`
for a terminal state that differs from one you hold, and `403`/`404` for a
call about a run you cannot match to the caller. For everything else — a spent
registration token, a credential that belongs to another runner, a wrong
method — what is checked is that the envelope and its `next_action` are there,
not which number carries them. Pick sensibly and be consistent; nothing will
fail you for choosing `401` over `403`.

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
| `live_sessions` | a run whose `session.mode` is `live` — offer one only to a runner that advertises it. The spec lists `live` as an enum value and connects it to no feature, so this pairing exists only here |

Send a gated control to a runner that does not advertise its feature and
nothing happens, for ever. `yad hub` answers such a request `409` rather than
queueing it.

## Proving it

```
yad conformance <hub url> --token <token> [--second-token <token>] [--harness id] [--lease-wait d]
```

The URL you give it is your base: every path in the spec is relative to it, so
a hub mounted at `https://example.com/yad/v1` is named in full. The spec's
`servers` entry says `/v1` because that is where `yad hub` mounts it; yours is
wherever you say it is.

Thirty-six black-box checks against a URL, written from §2 rather than from
`yad hub`'s internals — nothing in the suite imports the hub, so it tests the
protocol and not one implementation of it. A failure gives you the rule as a
sentence and the part of §2 it is written in, because whoever reads it is
implementing a hub and does not have this repository open.

**What it does to your hub.** It spends the registration token you give it and
registers a runner advertising a harness no real run asks for, so a suite
pointed at a live hub is offered nothing anyone was waiting on.

**Queue at least two runs for that harness first, and be able to offer both at
once.** The lease rules need two runs held simultaneously — one to report on,
one to leave unrenewed until its lease lapses — so one run is not enough, and
**two offered one at a time is also not enough**. Offering one run per sync
breaks no rule in §2 and is a perfectly good hub; it simply leaves those checks
unreachable, and the suite says so rather than failing you. More than two does
no harm.

Without them those checks are **skipped**, and each skip says what to queue to
make it possible.

**Give it a second registration token to check who holds a run.**
`--second-token` spends it on a second runner, which sends a batch of events
and a result for the run the first runner holds. Both must be refused, and a
`403` must carry `not_holder`, the code a runner stops on. Without the second
token those two checks are skipped, saying which flag would make them possible;
the half that needs one runner — a run you cannot match to the caller at all
is refused — is checked either way. Only one of the two tokens can be `-`.

**A skip is not a pass.** It is the suite saying your hub gave it no way to
ask. Read the skips before believing the passes.

### What it does not check

The suite prints this list itself, and it is eight rules — not a footnote. Each
is something **your hub still has to get right** with nothing to catch you:

| rule | why the suite cannot reach it |
|---|---|
| `POST /runners/{runner}/deregister` — held runs lost, offers requeued, the runner's sessions closed and their queued runs ended | deregistering retires the runner every other check is made as; the second runner `--second-token` registers could carry it, and does not yet. `yad hub` implements it; implement it in yours |
| The controls — `cancel`, `interrupt`, `steer`, `close_session`, `drain` — their repetition until the runner acts, and a `steer` being delivered once | nothing in v1 lets a *runner* ask for a control, so the suite can only wait for one it cannot cause |
| `start_at`, `min_version`, and the feature gates on `drain`, `steer`, `interrupt`, `close_session`, `start_at` | each needs a run or control the protocol gives a runner no way to request. The other half *is* checked: that a hub sends no control it should have gated |
| Sessions staying put — first claim binds the session to that runner, later runs to that runner alone, one at a time | needs two runs in one session, which only your own queueing can arrange |
| Capacity shared round the hubs, one pool, a unit at a time | a rule about one runner across several hubs, so no single hub can pass or fail it |
| The caps on tool output, event text and final text, and halving a batch a proxy refused | those bind the runner, not the hub |
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
