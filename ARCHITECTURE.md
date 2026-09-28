# Architecture

What YAD is made of, what it says on the wire, and how it drives a harness.
[`DOMAIN.md`](DOMAIN.md) owns the words; this file owns the shape; the *why* of
every choice below that was hard to reverse is a record in
[`docs/decisions/`](docs/decisions/). Where a section and a record disagree, the
record is older and this file is stale. The protocol's contract — what a hub
must do — is [`HUB.md`](HUB.md), beside the generated
`protocol/v1/openapi.yaml`; §2 here is its shape and its reasons.

## §0 The problem, stated once

Slava's systems can decide that some intelligent work should happen — implement a
ticket, review a PR, watch production logs, answer a question — and none of them
can make it happen anywhere but the one machine it is installed on.

- **Zumino** holds work and a queue and has no executor at all.
- **Yashiki** runs `claude -p --resume` on one Mac mini, for one household.
- Any Zumino *customer* should be able to attach their own machine and have
  Zumino's work run there, on their own subscriptions.

So: one small binary, installable anywhere, that advertises what it has, pulls
work from any number of hubs, runs harnesses against durable sessions, survives
usage limits and restarts, and reports everything it did.

YAD is not a tracker, not a UI, not an orchestrator and not a sandbox. It claims
a run, runs a harness, streams what happened, and says whether it is alive.

## §1 Shape

```
   ┌──────────┐   ┌──────────┐   ┌──────────────┐
   │  Zumino  │   │ yashiki  │   │   yad hub    │     hubs — anything hosting
   │ (embeds) │   │ (embeds) │   │ (standalone) │     the server half of v1
   └────▲─────┘   └────▲─────┘   └──────▲───────┘
        │ HTTPS, outbound only, from each runner
        │ register · sync · events · result
   ┌────┴──────────────┴────────────────┴────────┐
   │ runner (one profile)                        │
   │  connections ─ capacity pool ─ run executor │   runner core
   │  session store · event spool · outbox       │   (SQLite, one file)
   │  workdirs · accounts · control socket       │
   ├─────────────────────────────────────────────┤
   │ adapters: claude (stream-json) · codex      │   one per first-class harness
   │           (app-server) · fake (tests)       │
   ├─────────────────────────────────────────────┤
   │ supervisor: process group · watchdogs ·     │
   │             cancel ladder · env scrub       │
   └─────────────────────────────────────────────┘
        claude · codex · gh · git · docker …      host
```

- **Multi-homed runner, hub as a role** — [0003](docs/decisions/0003-hub-is-a-role-runner-is-multi-homed.md).
- **No listening port**; one Unix socket for the local CLI — [0004](docs/decisions/0004-runner-listens-on-no-port.md).
- **Pull by periodic sync** — [0005](docs/decisions/0005-pull-by-periodic-sync.md).
- **A runner holds nothing it did not claim** — [0008](docs/decisions/0008-runners-hold-no-schedules.md).
- **The owner's environment is the trust boundary** — [0015](docs/decisions/0015-owner-environment-is-the-trust-boundary.md).

### Packages

```
cmd/yad/                 the CLI — one file per command group, no logic
protocol/v1/             the wire types; public, so Go hubs can import them;
                         openapi.yaml generated from them and committed
protocol/hubapi/         yad hub's service API types, and its own generated
                         openapi.yaml — not part of the protocol (0022)
internal/config          profiles, config.toml, credentials on disk
internal/store           SQLite: schema, migrations, sqlc-generated queries
internal/harness         the catalog, detection, versions
internal/capability      the capability document and its fingerprint
internal/hostool         probing host tools (git, gh, docker) and how far each works; the binary a run uses (0045)
internal/probe           what both share: which binary an override or PATH names, and the words for what is wrong with it (0044)
internal/account         accounts per harness, homes, limit state, failover
internal/service         yad service: launchd agent and systemd user unit, login PATH (0028)
internal/adapter         the Adapter interface and event normalisation
internal/adapter/claude  stream-json both ways
internal/adapter/codex   app-server JSON-RPC
internal/adapter/fake    a scripted harness for tests
internal/supervise       spawn, process groups, watchdogs, cancel ladder
internal/workdir         sources, bare caches, worktrees, setup hook, slots, GC
internal/runner          connections, sync loop, capacity, executor, spool, outbox
internal/hubclient       the runner side of the protocol
internal/hubapiclient    the caller side of yad hub's service API
internal/hub             `yad hub`: huma server, store, submit/watch API
internal/control         the Unix control socket, server and client
internal/upgrade         `yad upgrade`: releases fetched over HTTPS, checksum, atomic replace
internal/conformance     the protocol conformance suite, run against any hub
internal/shellword       every command yad prints for pasting, built from argv
                         and POSIX-quoted; shellwordtest runs one through sh
```

Dependencies point downward only: `cmd` → `runner`/`hub` → everything else;
`adapter/*` never import `runner`; `protocol/v1` imports nothing of ours.

## §2 The protocol — v1

JSON over HTTPS. The Go types in `protocol/v1` are the source; `openapi.yaml`
beside them is generated and committed, and a test fails when they drift —
[0017](docs/decisions/0017-protocol-types-are-the-source.md). A field's
description reaches the document through its `doc:` struct tag; huma does not
read Go comments, so a field documented only in a comment generates none.
Every field a hub reads or writes carries one, written for someone generating
a client who will read nothing else. Nothing in the protocol is
harness-specific: a hub never learns what a rollout file is.

**[`HUB.md`](HUB.md) is the contract, and this section is the why.** A hub is
built from HUB.md and the document alone — Zumino's is built outside this
repository — so every rule a hub must follow is written there in full, with
an endpoint-by-endpoint contract, the state machine and a checklist. What
follows is the protocol's shape as the runner sees it and the reasons behind
it, each part naming the HUB.md section that states its rules. Where the two
disagree, HUB.md is the one a hub was built from: fix whichever is wrong, and
never leave them apart. `yad conformance` files each rule it checks under the
HUB.md section that states it, and a test fails when one of those headings
moves.

A **connection** is a base URL — `https://zumino.cc/api/yad/v1` — and every path
below is relative to it, so a hub can mount the protocol anywhere.

### Calls

Contract: [HUB.md §3](HUB.md#3-the-calls), one call at a time, and
[§10](HUB.md#10-errors-and-next_action) for errors.

| | |
|---|---|
| `POST /runners/register` | registration token → runner credential; full capability document in, hub features and timings out |
| `POST /runners/{runner}/sync` | the periodic call: state and health in; runs, control messages and the next interval out |
| `POST /runs/{run}/events` | a batch of events, idempotent by `(run, seq)`; answers `acked_through` |
| `POST /runs/{run}/result` | the terminal state, idempotent; retried from the outbox until acknowledged |
| `POST /runners/{runner}/deregister` | the credential dies; the hub marks held runs lost, requeues its offers, and closes its sessions, ending the runs queued in them |

Every request carries `Authorization: Bearer <runner credential>` (the
registration token, for `register` only), `Yad-Protocol: 1` and
`User-Agent: yad/<version>`. Errors are `{"error": {"code", "message",
"next_action"}}` — the next action is mandatory, because a runner on a
customer's machine is debugged by reading it. The codes v1 names are the
constants in `protocol/v1/error.go`; a hub's own fault is `internal`, with a
`5xx`, and a runner retries it as it retries any `5xx`.

A registration token registers one runner once. The exchange kills it, and the
same token again — for that runner or for another — is refused: runner ids are
not secret, so a token that stayed usable would hand a runner's sessions, and
the grants delivered into them, to whoever else had it.

Two wire rules every hub must follow, both because TypeScript hubs generate
strict types from `openapi.yaml`:

- **An absent list is an empty list.** Lists that can be empty are omitted
  rather than sent as `[]` or `null`; `runs` and `controls` below are optional
  on the wire.
- **Unknown fields are ignored.** Objects allow additional properties, so a
  field added within v1 never breaks an older hub or runner.

A request whose `Yad-Protocol` is missing or names another version is refused
with `426 unsupported_protocol` before its body is read. Every response under the
base — an unknown path, a wrong method — carries the error envelope, never a
plain-text 404 or 405.

### Sync

Contract: [HUB.md §3](HUB.md#post-runnersrunnersync) for the call and the order a
hub handles it in, [§4](HUB.md#4-runs) for what may be offered,
[§5](HUB.md#5-leases-timings-and-runners-that-go-away) for leases and timings,
[§7](HUB.md#7-controls-and-features) for controls and
[§8](HUB.md#8-sessions) for sessions.

```
→ { runner_id, fingerprint, capabilities?,        // document only when asked
    health: { load?, free_capacity: {total, by_harness}, disk_free_bytes?,
              harnesses: [{id, ready, accounts: [{label, state?, limited_until?,
                            windows?: [{name, used_percent, resets_at?}]}]}],
              spool_depth?, outbox_depth?, recent_errors[], draining? },
    runs: [{ run_id, state, resumes_at?, reason? }],    // every run held
    closed_sessions: [{ session_id, reason, closed_at }],  // until answered
    logins: [{ login_id, harness?, account?, method?, state,
               url?, user_code?, error?, updated_at }] }    // until answered
← { next_sync_ms, lease_ms,
    runs: [Run],                                  // never more than free capacity
    controls: [{ kind, run_id?, session_id?, text?,
                 login_id?, harness?, account?, code?, token? }],
    min_version? }
```

- **Account state**: `free`, `limited` (until `limited_until`) or `needs_login`
  — a home whose login the owner has to finish. The field is optional: a runner
  from before it omits it, a hub must not refuse one that does, and absent
  means the runner cannot say rather than that anything is wrong. A limited
  account and one that needs login are both skipped for runs; `ready` is
  whether a harness has a free account, or no accounts at all and a login in
  its own default home, which its runs then use
  ([0053](docs/decisions/0053-a-harness-on-its-own-login-is-checked-like-an-account.md)).
  A harness with no accounts is a state, not a failure.
- **Account windows**: each account's usage windows, by the harness's own name
  — Claude's `five_hour` and `seven_day`, Codex's `primary` and `secondary` —
  with `used_percent` (0-100, whatever scale the harness reported) and
  `resets_at` where it said. Reported from every run, limit or not, so a hub
  sees an account running low before it runs out. Absent means no run has yet
  heard a window, never that the account has no limits; a window the harness
  did not mention keeps its last value rather than reading zero.
- **Control kinds**: `cancel`, `interrupt`, `steer`, `close_session`, `drain`,
  `report_capabilities`, `update` (reserved —
  [0018](docs/decisions/0018-no-self-update-in-v1.md)), the four of hub
  login: `start_login`, `login_code`, `login_token`, `cancel_login`, and
  `remove_account`.
- **Controls are not acknowledged**, so a hub repeats `cancel` and `interrupt`
  in every response to a sync listing the run, until the run ends; the runner
  acts on the first. A `steer` is sent once — twice, the harness would read it
  twice. An interrupt that reaches a run before its harness is up ends it as a
  cancel does, with nothing spawned —
  [0025](docs/decisions/0025-a-cancel-is-repeated-and-an-answer-that-landed-stands.md).
  That includes the answer that acknowledges the claim: a control there rides
  in the run's claim to the executor rather than arriving ahead of it (DEV-113).
  A cancel in that answer withdraws the claim; an interrupt starts the run
  already stopped, so it ends `cancelled` with a result, and a steer waits
  for the harness.
- **Drain** — [0029](docs/decisions/0029-drain-is-a-three-signal-ladder.md).
  A hub sends `drain` only to a runner advertising the `drain` feature, and
  repeats it until a sync's health says `draining`. A draining runner declares
  no free capacity, keeps listing what it holds and is offered nothing; it
  exits once its runs have ended.
- **Closing sessions** — [0035](docs/decisions/0035-a-runner-reports-every-close-in-its-sync.md).
  A runner advertising `close_session` lists every session it closed —
  `reason` is `closed`, `closed_by_owner`, `expired` or `disk_pressure` — in
  each sync until one carrying it is answered. A hub sends `close_session` only
  to such a runner and repeats it until the session appears there; a session
  with a run held closes when that run ends. A run offered in a closed or
  closing session is refused with class `session_closed`.
- **Hub login** — [0055](docs/decisions/0055-a-hub-may-log-an-account-in-by-link-or-by-token.md).
  A hub sends the four login controls only to a runner advertising `login`.
  `start_login {login_id, harness, account?}` runs the harness's own login on
  the machine and the runner reports its link as `url` while `waiting` — for
  Codex, with `user_code`, the device code the owner types there, and nothing
  comes back ([0057](docs/decisions/0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md));
  `login_code {login_id, code}` carries the code the owner got there, never to
  a login reporting `user_code`;
  `login_token {login_id, harness, account, token}` stores a
  `claude setup-token` token as the account's login; `cancel_login {login_id}`
  ends one. The runner reports each login in `logins` — `starting`, `waiting`,
  `checking`, then `succeeded`, `failed`, `expired` or `cancelled` — in every
  sync until one carrying its end is answered, as closed sessions are; a hub
  repeats each control until those reports answer it, and ends `failed` a
  login the runner reported and then leaves out (it restarted). A hub ends a
  login on its own word only while no answer has carried it, and must end one
  sent and unheard for thirty minutes `failed`, blanking its token; a
  runner's later report of an end replaces the hub's. `code` and
  `token` are secrets: logged by neither side, never reported back, and a
  token held by the hub only until the runner reports its login.
- **Managing accounts** — [0057](docs/decisions/0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md).
  A runner advertises `accounts` per connection: only to a hub whose
  connection the owner has not set `manage_accounts = false` on, and only
  beside `login`, so each hub's fingerprint is its own. To such a hub alone,
  `start_login` and `login_token` may carry `add`, which creates the account:
  the runner lists it once the login takes, and a login that ends any other
  way lists nothing. `remove_account {harness, account}` removes one as `yad
  account remove` does; a hub repeats it until neither the runner's
  capability document nor the sync's health lists the account (the document
  decides: health names only harnesses the runner can drive, and at most
  sixteen accounts of each), or the runner stops advertising `accounts`.
  An added account is the machine's, and every hub's runs rotate through it.
- **Claim by listing.** A run offered in a sync response is claimed when the
  runner lists it in its next sync. An offered run that the next sync does not
  list was never received, and the hub offers it again. The runner takes the
  capacity *before* it syncs, so it can always start what it is offered.
- **Capacity goes round the hubs** — [0005](docs/decisions/0005-pull-by-periodic-sync.md).
  One pool for every connection. A sync takes what is free up to its
  connection's share of the whole capacity, less what it already holds; the
  share is the capacity dealt a unit at a time around the ring of connections
  from a cursor that stays put for the whole fill and moves on one place once
  the pool is full. So a hub with a deep queue cannot crowd out a quieter one:
  two hubs that both want everything hold half each, three over four units
  hold two, one and one whatever order they sync in, the extra unit of an
  uneven division goes round the ring from fill to fill, and a unit freed goes
  to whoever's turn it is rather than to whoever asks first: what is free is
  dealt from the same cursor to the connections short of their share, so a hub
  that ends a run and syncs again at once cannot take the unit back from one
  still waiting. Each harness's `cap` is dealt the same way, from a cursor of
  its own, and a sync advertises for that harness only its connection's turn
  at the cap, held for it like the rest of its reservation until the sync
  ends — so two hubs that both want only Claude under a cap of two run
  one each, and neither can refill the cap every time its own run ends. A
  connection that leaves units unused has no work for them and is out of the
  deal — keeping what it holds — until its next sync asks again, so one busy
  hub still fills a pool the others have no queue for. Units left over because
  a harness's cap gave the sync none are not that: the hub may have had only
  that harness's work, so the connection keeps its turn at the cap, and at as
  much of the pool as the cap has free for it. One given room at a harness
  that spent the sync on other work passes on that harness until its cap next
  fills, so a hub whose queue starts with other work does not hold a turn at a
  cap it never uses. A unit held for a turn its hub turns out not to want waits
  at most until that hub's next sync. The owner's `cap` on a
  connection bounds what a sync *asks* for and is checked nowhere else: a run
  over it is never claimed and then found to be over it.
- **Start on acknowledgement** — [0019](docs/decisions/0019-a-run-starts-once-its-claim-is-acknowledged.md).
  The runner starts a run only after a sync listing it has been answered without
  a `cancel` for it, and syncs again at once when offers arrive, so a run starts
  one round trip after it is offered. A listed run the runner does not hold is
  answered with `cancel`. A run the runner will not take is reported as a
  `failed` result with error class `refused`.
- **Sessions stay put.** The first claim in a session binds it to that runner;
  its later runs are offered to that runner alone, one at a time.
- **Lease.** Every sync renews the lease on every run it lists. A run whose lease
  lapses (default: four missed intervals) is **lost** on the hub's side. The
  lease a hub names is never shorter than the interval it names beside it: a
  shorter one lapses on a runner that synced exactly when it was asked to, so
  the hub takes back the runs of a runner doing everything right. The four
  intervals are the default and one interval is the floor — they do not
  compete, and a hub may hold to fewer than four as long as it holds to one.
  An offer carries the same lease: one not claimed within it goes back in the
  queue for any runner, and a claim listed after that is answered with
  `cancel`, as a lapsed claim is —
  [0046](docs/decisions/0046-a-silent-runner-loses-its-offers-with-the-lease-and-its-sessions-after-a-day.md).
- **Abandon after.** A runner that has not synced for longer than the hub's
  abandon-after (default a day, and always longer than the lease) has every
  session bound to it closed and the runs queued in them ended. Its credential
  stays: its next sync is answered normally, with `close_session` for each of
  those sessions until it reports the close.
- **Timings belong to the hub**: default interval 15 s, bounded 5–60 s; the
  runner adds ±10 % jitter and backs off 1 s → 30 s on errors.

### Run

Contract: [HUB.md §4](HUB.md#4-runs), and [§9](HUB.md#9-grants) for grants.

```
{ run_id, session: { id, new, mode: "per_run" },
  harness, model, effort?,
  brief: { context, instruction },
  sources: [{ git: { url, base, branch } } | { path }],
  grants: [{ name, value, as: "env" | "file" }],
  start_at?, max_wait_ms?, wall_clock_ms?, inactivity_ms? }
```

`session.mode = "live"` is reserved and refused until a runner advertises it.
`effort` is how hard the harness thinks, in its own terms, as `model` is — a
string and never an enum; the runner passes it through unchecked and the
harness refuses a level it does not take
([0049](docs/decisions/0049-a-runs-effort-is-the-harnesss-word.md)).
A grant's `name` is any valid environment variable name except `PATH`, `HOME`,
`LD_*` and `DYLD_*`, and except the variables that choose a harness's
credential or home (`ANTHROPIC_API_KEY`, `CLAUDE_CONFIG_DIR`, `CODEX_HOME` and
the rest of `accountGrantNames` in `protocol/v1/grant.go`), all in any case; no
two grants share a name, and no two `file` grants have names differing only by
case — one file on a case-folding filesystem. The schema cannot say any of
that, so both sides check it and a run breaking it is refused whole
(`Run.Validate`, decisions 0038 and 0040).

### Run states

Contract: [HUB.md §4](HUB.md#run-states), which adds the two states only a hub
has, `queued` and `offered`, and the error classes a hub acts on.

```
claimed ─► preparing ─► running ─► succeeded | failed | cancelled | timed_out
                          │  ▲
                          ▼  │ (limit resets / account frees)
                        waiting ──────────────────────────► timed_out (max_wait)
   any non-terminal state ── runner lost ──► lost      (decided by the hub)
```

`preparing` covers the workdir, the setup hook and the account; a run reports
`running` only once its workdir exists (Multica #3999). A run also reaches
`waiting` from `preparing` — no account was free before the turn started — and
comes back through it either way: the resumed run prepares its workdir again,
because a park gives up the locks a live process was holding. Non-terminal states
travel in syncs; the terminal one travels in the result. A finished run whose
result is not yet acknowledged stays listed, as `running`, so its lease outlasts
a hub outage — [0023](docs/decisions/0023-lost-stands-against-a-late-result.md).

### Events

Contract: [HUB.md §6](HUB.md#events).

```
{ seq, at, kind, text?, tool?: { id, name, input?, output?, truncated?, is_error?, exit_code? },
  status?, usage?: { model, input, output, cache_read, cache_write, cost_usd? },
  error?: { class, message } }
```

`kind` is the closed set in `DOMAIN.md`. Batches go out every second or every
100 events, from the SQLite spool, so a network loss drops nothing; the hub's
`acked_through` — the highest `seq` up to which it holds every event — is
authoritative, and the runner resends after it. Tool output is capped at 8 KiB
per event; the runner caps text, error messages and a result's final text at
1 MiB each, so one event or one result stays well under `yad hub`'s 16 MiB body
limit. A proxy with a limit near 1 MiB can still refuse an outlier; the runner
halves a refused batch down to one event and then keeps retrying it. Only the runner a run was claimed by may append to it, before or
after it ends; anyone else gets `403 not_holder`, or `404 not_found` from a
hub that will not name the run to a runner that does not hold it.

### Result

Contract: [HUB.md §6](HUB.md#results), including the table of how a runner
treats each answer.

```
{ state, final_text?, error?: { class, message },
  usage: { by_model: {...} },
  metrics: { duration_ms, first_event_ms, tool_calls, api_retries, stalls,
             cancel_latency_ms?, waited_ms?, account_switches },
  last_seq }
```

Written to the outbox, in the same transaction as the run's terminal state,
before the first attempt; sent once the run's events are all acknowledged;
deleted on a 2xx, retried with backoff to five minutes and replayed at every
start. A `409 conflict` means the hub already has a different terminal state —
`lost`, when the lease lapsed first — and the runner keeps the hub's
([0023](docs/decisions/0023-lost-stands-against-a-late-result.md)). `403
not_holder` and an `invalid` refusal are final too. Anything else — a `404`
that may be a wrong URL, a `401`, a `5xx`, a proxy's bare 4xx — is retried; an
events batch refused as too large — by the hub or a proxy — goes again in
halves. The hub applies
a result from the runner the run was offered to or claimed by, once; the same
state again is acknowledged.

### Versioning

Contract: [HUB.md §11](HUB.md#11-versioning) and
[§7](HUB.md#7-controls-and-features) for the features.

The major version is in the path. Within it, both sides advertise feature
strings — the runner in its capability document, the hub in its register
response — and nothing is used that the other side did not advertise: a hub
sends `drain`, `close_session`, `steer` and `interrupt` only to a runner
advertising each, the four login controls only to one advertising `login`,
`remove_account` and a login carrying `add` only to one advertising
`accounts` — which a runner advertises per connection,
and offers a run carrying `start_at` whose moment is still
ahead only to a runner that will hold it back rather than start it at once —
once the moment has passed there is nothing to hold, and the run goes to any
runner, or it would wait for ever on a fleet without the feature. A run
carrying an `effort` goes only to a runner advertising `effort`, for as long
as that takes: any other would run the harness at its default and say nothing. A runner
whose fingerprint moved without the document it promised is treated as
advertising neither, until the document it is asked for arrives. `yad hub` advertises no
`hub_features` of its own — it has nothing beyond the v1 baseline.

Every enum in the document is closed for all of v1
([0047](docs/decisions/0047-four-v1-protocol-rules-settled-by-the-clean-room-check.md)):
a hub validating against it refuses a value outside the set, and a runner drops
a batch refused as invalid, so a value added to one goes only to a side that
advertised the feature adding it — a new control kind to a runner advertising
it, a new event kind, run state, result state or close reason to a hub
advertising it in `hub_features`. A yad runner keeps no `hub_features` today,
because v1 defines none; the first value gated on one needs the runner to keep
them per connection first. `protocol/v1`'s `TestTheV1EnumsAreClosed` pins every
set as v1 shipped it. The enums a hub sends are generated as
`x-extensible-enum`, so `make check-breaking` accepts their gated growth. The
service API's enums stay `enum`, because it has no features to gate on
([0058](docs/decisions/0058-an-enum-a-hub-sends-is-written-as-x-extensible-enum.md)).

A hub may refuse a runner below `min_version` with `version_too_old` and a next
action. `yad hub serve --min-version 0.4.0` sets that floor: a runner under it
is refused at register — before its registration token is burned, so the
upgraded runner can still use it — and at every sync, with the next action
`yad upgrade`. The floor rides in the register and sync responses as
`min_version`, so a runner can say what it is being asked for. A runner refused
mid-run stops syncing, so the runs it holds stop renewing and the sweep records
them `lost` — raising the floor on a working fleet is a drain first
([0023](docs/decisions/0023-lost-stands-against-a-late-result.md) makes `lost`
final, so a result that lands afterwards is refused). Versions are
compared on the release core alone, because `git describe` writes
`v0.4.0-4-gabc1234` for a build four commits *after* v0.4.0, which semver would
sort before it; a version neither side can parse — an unstamped `dev` build, a
mistyped floor — is never refused.

### yad hub's service API

Not part of the protocol, and never implemented by a hub that embeds it:
`yad hub`'s own way for a service or a person to make runs —
[0022](docs/decisions/0022-hub-service-api-beside-the-protocol.md). Mounted at
`/api/v1` beside the protocol, described by `protocol/hubapi/openapi.yaml`
(generated, committed, drift-checked), authenticated by an **admin token**.

| | |
|---|---|
| `POST /runs` | queue a run: harness, model, brief, optional sources, grants and session; idempotent by `run_id` |
| `GET /runs/{run}` | the run's hub-side state (`queued`, `offered`, then the protocol's) and its result; never its grants |
| `GET /runs/{run}/events?after=N&wait_ms=…` | long poll: events after `N`, the run, and `done` once the stream is complete |
| `POST /runs/{run}/cancel` | a run no runner started ends `cancelled` here; a held one gets a `cancel` control, and shows `cancel_requested_at` until it ends |
| `POST /runs/{run}/interrupt` | an `interrupt` control for a held run; 409 before it starts, and for a runner without the `interrupt` feature |
| `POST /runs/{run}/steer` | a `steer` control with `{text}` for a held run, sent once; 409 before it starts, and for a runner without the `steer` feature |
| `GET /runners` | every runner this hub knows, newest sync first, each with the health its last sync carried — load, free capacity, disk, per-harness readiness with each account's state and windows, spool and outbox depth, recent errors |
| `GET /runners/{runner}` | one runner, the same view |
| `POST /runners/{runner}/drain` | a `drain` control, repeated until the runner says it is draining; 409 for a runner without the `drain` feature |
| `GET /sessions/{session}` | the session: its runner, and `open`, `closing` or `closed` with the reason |
| `POST /sessions/{session}/close` | a session no runner holds closes here, its unstarted runs cancelled; a held one gets `close_session` until its runner reports it closed; 409 for a runner without the `close_session` feature. A closing or closed session takes no new run |
| `POST /runners/{runner}/logins` | a hub login for `{harness, account?, add?}`: by link, or by token with `token` — `requested` until the runner's next sync takes it; 409 for a runner without the `login` feature, and for an `add` to one without `accounts`. A newer login for the account replaces an older one |
| `GET /runners/{runner}/logins/{login}` | the login's state, its `url` (and for Codex its `user_code`) while `waiting`, and the runner's `error` at an end; never its code or token |
| `POST /runners/{runner}/logins/{login}/code` | `{code}` for a `waiting` link login, delivered at the next sync; 409 before the link is out, after the end, or for a device-code login (one showing `user_code`) |
| `POST /runners/{runner}/logins/{login}/cancel` | a `cancel_login` until the runner reports the login over |
| `POST /runners/{runner}/accounts/{harness}/{account}/remove` | a `remove_account` until neither the runner's capability document nor its health lists the account, or it stops advertising `accounts`; ended by a login that adds the account again; 409 for a runner without the `accounts` feature |
| `GET /runners/{runner}/accounts/{harness}/{account}` | whether the runner's last health names the account and in what state, and a removal still waiting |

## §3 Running a harness

A run is: take capacity → prepare (workdir, setup hook, account) → spawn →
stream → reap → report.

### Adapters

```go
type Adapter interface {
	Harness() string
	Start(ctx context.Context, spec Spec) (Turn, error)
}

type Turn interface {
	Events() <-chan protocol.Event
	Steer(text string) error
	Interrupt() error
	Terminate() error        // SIGTERM to the process group: the ladder's second rung
	NativeSessionID() string // as soon as the harness has one, for pinning
	Wait() Outcome // terminal state, final text, usage, native session id, limit
}
```

**Claude** — [0006](docs/decisions/0006-claude-by-stream-json-codex-by-app-server.md):

```
claude -p --input-format stream-json --output-format stream-json --verbose
       --include-partial-messages --replay-user-messages
       --disallowed-tools AskUserQuestion --system-prompt-snapshot off
       --permission-mode <owner config>
       (--session-id <uuid> | --resume <uuid>) [--model m] [--effort level]
       [--append-system-prompt-file <context file>]
```

The instruction is one stream-json `user` frame on stdin, written from its own
goroutine; stdin stays open for `control_request` (interrupt) and steers until
the last `result`, and closing it is what lets Claude exit. YAD chooses the
session id, so nothing has to be scraped; an echoed id that differs means the
resume silently failed, and the run fails with `session_mismatch`. A resume
Claude refuses — no transcript for the id — fails with `resume_rejected`
([0031](docs/decisions/0031-a-failed-resume-is-the-hubs-to-decide.md)).
The brief's context is appended to the system prompt on every run, and
`--system-prompt-snapshot off` makes Claude render that prompt afresh rather
than replay the one it recorded on the session's first request — without it a
resumed run's context never reaches the model
([0050](docs/decisions/0050-a-runs-context-reaches-the-harness-on-every-run.md)).
The capability probe asks the installed claude's `--help` for every flag a run
passes, and a Claude lacking one is reported with an error — not drivable —
saying to run `claude update`.
`AskUserQuestion` is disallowed — headless, it returns an empty answer. Claude
does not refuse an `--effort` it does not know: it warns on stderr and runs at
its default, so the adapter watches stderr for that warning until Claude's
first frame of work, stops the turn, and fails the run with it
(`harness_error`). A steer
is another `user` frame: Claude reads it at the next tool boundary, or answers
it as a follow-up turn in the same process; `--replay-user-messages` echoes
each frame as it is taken, which is how the adapter knows which result is the
last. The outcome rules and the rest are
[0021](docs/decisions/0021-claude-runs-end-at-the-last-result.md).

The permission mode is the owner's `permission_mode`, and `bypassPermissions`
when unset: a run is unattended and auto-approves
([0015](docs/decisions/0015-owner-environment-is-the-trust-boundary.md)), and
Claude's own default would deny every tool that needs a prompt. Claude refuses
`bypassPermissions` as root unless `IS_SANDBOX=1`, so the adapter refuses such a
run before spawning, with the way out. Only the owner declares the sandbox, in
the runner's own environment: YAD never sets `IS_SANDBOX`, and strips it from a
run's environment, which carries the hub's grants.

**Codex** — [0006](docs/decisions/0006-claude-by-stream-json-codex-by-app-server.md):
`codex app-server --listen stdio://`, JSON-RPC over stdin and stdout
(`internal/adapter/codex/rpc.go`): `initialize` → `initialized` →
`thread/start`, or `thread/resume` with the stored thread id → one `turn/start`
with the instruction and the run's `effort`, when it has one; the brief's
context is the thread's `developerInstructions`, and on a resume it is also
put in the thread as a developer message (`thread/inject_items`) before the
turn, since Codex reads a resume's instructions only after a compaction
([0050](docs/decisions/0050-a-runs-context-reaches-the-harness-on-every-run.md)). Codex's model list is read
from `models_cache.json` in each home a run may use, for the capability
document. Each of those is answered within 30 s. The thread id is
the native session id, exposed the moment `thread/start` answers. Codex writes
subagents' threads to the same pipe and a resume replays the thread's history,
so only notifications naming the run's thread and, once it has started, the
run's own turn are read. Only `turn/completed` decides the run; a steer is
`turn/steer` into the same turn, an interrupt `turn/interrupt`; once the turn is
over input closes and the app-server gets 2 s to exit. A hub login drives
the same app-server's device-code login instead of a thread (hub login,
below). A resume Codex has no
rollout for fails with `resume_rejected`, and one that resumes another thread
with `session_mismatch`. The rest is
[0037](docs/decisions/0037-a-codex-run-is-its-own-turn-and-its-protocol-is-pinned.md).

The approval policy and sandbox are the owner's `approval` and `sandbox`, sent
with every `thread/start` and `thread/resume`, and `never` and
`danger-full-access` when unset — unattended, and the owner's machine is the
boundary, as for Claude. A request for approval that still arrives is declined
([0036](docs/decisions/0036-codex-runs-unsandboxed-and-never-asks-unless-the-owner-says.md)).

The app-server is marked experimental and Codex ships weekly, so the protocol is
pinned: the adapter's slice of `codex app-server generate-json-schema` is hashed
per recorded version, and an installed codex whose slice differs is still
driven, with a warning on the harness in the capability document and in
`yad doctor`.

**Fixtures.** Every adapter test replays recorded JSONL named by harness version
(`internal/adapter/claude/testdata/claude-2.1.276/*.jsonl`,
`internal/adapter/codex/testdata/codex-0.147.0/*.jsonl` — both directions of
the conversation, ours wrapped as `{">": …}`) through a fake harness process.
Recording new ones is a manual step, behind a build tag (`YAD_REAL_HARNESS=1 go
test -tags realharness -run TestRecord ./internal/adapter/claude/`, and the same
for `codex`); the suite never runs a real harness.

### Supervisor

- **One spawn point.** Every child — harness, git, setup hook, `--version` probe —
  starts through `supervise.Start`: its own process group, a scrubbed environment
  (`CLAUDECODE`, every `CLAUDE_CODE_*`, `ANTHROPIC_API_KEY` unless configured,
  anything `YAD_*`), and a stderr tail kept at 2 KiB. A run's grants are added
  after the scrub, which is why a grant may not name `ANTHROPIC_API_KEY` or any
  other variable that chooses the harness's credential (0040). git and setup hooks start
  with `NoTTY` — a session of their own, no controlling terminal — so nothing
  they run can prompt. A host tool a run uses is the one detection resolved
  ([0045](docs/decisions/0045-runs-use-the-host-tools-detection-resolved.md)):
  yad's git is `hostool.Locate("git")`, and a harness and a setup hook get
  `<data>/host-tools/` first on `PATH`, holding a link named `git`, `gh` or
  `docker` for each tool only its `YAD_<ID>_PATH` finds. `Start` hands back the raw
  stdout pipe; the 32 MiB line cap belongs to the adapters' line reader
  (`adapter.LineReader`), which skips an oversized line and reports it rather
  than ending the run.
- **Cancel ladder**: the adapter's interrupt → 10 s → `SIGTERM` to the group →
  5 s → `SIGKILL` to the group. Descendants are killed even after the leader
  exits cleanly — they hold pipes and git locks. The executor climbs it around
  the event stream (`Turn.Interrupt`, `Turn.Terminate`, then the run's context),
  so what the harness says on the way down is spooled. A run waiting on its
  `start_at` or still preparing is cancelled without spawning. The result's
  `cancel_latency_ms` is from the control's arrival to the turn's end. A
  harness whose own result landed before the cancel reached it keeps that
  result — [0025](docs/decisions/0025-a-cancel-is-repeated-and-an-answer-that-landed-stands.md).
- **The way down** — [0029](docs/decisions/0029-drain-is-a-three-signal-ladder.md).
  The first stop signal, or the hub's `drain`, stops claiming: offers never
  listed are withdrawn, runs held go on, syncs go on. The drain wait
  (`[drain] wait`, default 30 min) running out, or a second signal, cancels
  every run held down the cancel ladder, each result `cancelled` with class
  `runner_stopping`. Once every run has ended the runner gives the spool and
  the outbox up to 30 s and exits. A third signal exits at once. The service
  units' stop timeout is derived from the drain wait at `yad service install`
  (`runner.StopBudget`), so a changed wait takes a reinstall.
- **Restart** — [0030](docs/decisions/0030-a-restart-reports-lost-and-replays-first.md).
  Every run a previous process began is reported lost (`runner_restarted`,
  `last_seq` its last spooled event) through the outbox, never run again; a
  claim the hub never acknowledged is withdrawn, with the session it opened,
  for the hub to offer again — the hub had bound no session to it, and sends
  the session's next run as new
  ([0047](docs/decisions/0047-four-v1-protocol-rules-settled-by-the-clean-room-check.md)).
  An acknowledged claim that never began is reported lost like the rest. The
  spool and the outbox are replayed before the first claim. Sessions keep their
  native id and workdir, so the next run resumes them.
- **Watchdogs**: inactivity on the event stream (owner default 30 min; a run may
  lower it) and an optional wall-clock cap. "Force-stopping a healthy run throws
  away the work" — the inactivity default errs long. A watchdog that fires
  climbs the same ladder, starting with the interrupt, which keeps the session
  resumable. The run is
  `timed_out`, with the error class `inactivity_timeout` or
  `wall_clock_timeout`, whatever the stopped harness said last.
- **Exit 0 is not success.** Only the harness's result event decides the state;
  `prompt_too_long` arrives with `subtype: success`.

### Sessions and workdirs

- The session store maps `session → (harness, native id, account, workdir,
  last used, state)`. The native id is written the moment it is known, not at the
  end — for Claude, at spawn — so a crash does not lose the resume pointer. A run
  in a session with a native id resumes it; one without starts the harness's
  conversation fresh. Last used is the end of the session's last run.
- **A failed resume** ends the run `failed`: `resume_rejected` when the harness
  has no conversation for the id, `session_mismatch` when it ran another one.
  Neither moves the pointer or closes the session; the hub decides —
  [0031](docs/decisions/0031-a-failed-resume-is-the-hubs-to-decide.md).
- **One live run per session**, enforced by the store. A run continuing a
  session this runner does not hold, or of another harness, is refused at the
  claim (`refused`, [0019](docs/decisions/0019-a-run-starts-once-its-claim-is-acknowledged.md));
  one in a closed or closing session with `session_closed`
  ([0035](docs/decisions/0035-a-runner-reports-every-close-in-its-sync.md)).
- **Workdir** per session under `<data>/workdirs/<connection>/<session>/`, kept
  across its runs and never deleted by one —
  [0032](docs/decisions/0032-a-workdir-belongs-to-its-session.md). The session
  id is the hub's, so it is kept only when it is a plain lower-case name and
  hashed otherwise; no hub-chosen id becomes a path. Git sources come from
  a bare cache per repository (`<data>/repos/<name>-<hash>.git`, fetched before
  every checkout) as a worktree on the run's branch — as it stands when it
  exists, else cut from `base`, else `yad/<connection>/<session>` from the default branch;
  a continuing session finds its worktree and branch as it left them, and a
  continuing run that names no sources is prepared from the session's own. One git
  source is the session's workdir itself; one `path` source is used in place,
  the harness running in it, under `flock`s held until the run ends —
  exclusive on it, shared on the directories above it;
  several lie side by side under the workdir. No sources → an empty directory.
  Hub strings are checked before git sees them, git never prompts, and a local
  source must resolve inside the owner's `[workdirs] roots` — with none
  configured, the owner's home directory
  ([0033](docs/decisions/0033-sources-reach-only-what-the-owner-allows.md),
  [0038](docs/decisions/0038-the-owner-trusts-the-hubs-it-connects.md)). A
  runner with no home directory to resolve reaches nothing.
  `internal/workdir` does all of it; the executor calls `Prepare` on the
  session's directory in `preparing`, spools what it reports as the run's
  first events, and fails the run with `source_refused`, `source_failed` or
  `setup_failed`.
- **Setup hook**: if the worktree has an executable `.worktree/setup`, it runs
  with `WT_ROOT`, `WT_MAIN` (the bare cache), `WT_BRANCH`, `WT_SLUG`, `WT_REPO`
  and `WT_SLOT` — the contract in [docs/setup-hooks.md](docs/setup-hooks.md) —
  under `[workdirs] setup_timeout`, its output a `tool_call`/`tool_result` pair
  capped at 8 KiB. It runs until it has succeeded once in a worktree; a hook
  that fails fails the run with `setup_failed`
  ([0034](docs/decisions/0034-a-failing-setup-hook-fails-the-run.md)).
  Slots are allocated per repository per runner, from 1, and recycled when a
  workdir is reclaimed (`workdir.Manager.Reclaim`).
- **Reclaiming** — [0011](docs/decisions/0011-hub-closes-sessions-runner-collects.md),
  [0035](docs/decisions/0035-a-runner-reports-every-close-in-its-sync.md):
  on `close_session`, on the owner's `yad sessions close`, on the idle TTL
  (default 14 days, reported as expired), and oldest-idle-first while the disk
  under `<data>/workdirs` is below `sessions.disk_floor` (default 5 GiB; a
  session idle under an hour is spared). Never a session with a run held,
  waiting ones included: its close waits for the run to end. A claim
  withdrawn before it started ends that wait too: the session it opened is
  deleted with it only when no close was asked for, and otherwise closes, so
  the close is reported and the run offered again is refused. The close is
  recorded first and the workdir removed by the collector's sweep —
  `workdir.Manager.Reclaim` first takes its worktrees out of their bare caches
  and frees its slots — which retries a removal that failed and touches
  nothing outside `<data>/workdirs`.
  The collector runs whether or not any hub is connected: the disk is the
  machine's, and a hub disconnected while no daemon ran leaves sessions no
  hub will ever close.
  A missing last-used timestamp is unknown, never ancient: the TTL counts from
  the sweep that first sees it. Every close goes to the session's hub in
  `closed_sessions`.

### Accounts and usage limits

[0013](docs/decisions/0013-accounts-fail-over-and-limited-runs-wait.md).

- Each account has a harness home: `<data>/accounts/<harness>/<label>/`, passed as
  `CLAUDE_CONFIG_DIR` or `CODEX_HOME`. `yad account add claude work` runs the
  harness's own login with that home, and adds `work` to `config.toml` only once
  the harness's own login check says the home is logged in; otherwise nothing
  is added and the home is kept for the next try. `--device` passes Codex's
  `--device-auth`, for a machine with no browser.
- **A token account** ([0054](docs/decisions/0054-a-claude-account-may-be-a-token-and-every-account-shares-the-machines-config.md)):
  `yad account add claude tl --token -` reads a `claude setup-token` token
  from stdin — never argv — and needs no terminal. It is kept in the home as
  `yad-oauth-token` (`0600`; one others can read is not used) and reaches the
  account's runs and its login check as `CLAUDE_CODE_OAUTH_TOKEN`, after the
  home variable, so a printed command never carries it. Claude's login check
  answers yes for any token, so a token is found wanting by the run it fails:
  a result with `api_error_status` 401 (`adapter.Outcome.AuthRejected`) parks
  the account in `needs_login` without asking — if the refused token is still
  the stored one (`account.TurnEnv` hashes the turn's). The login probe skips
  a token account, whose check would say yes to the refused token; every next
  action for one is `--token -`, and a plain `yad account add` on it runs the
  login and its check without the token (`account.LoginEnv`,
  `account.OwnLogin`), leaving it in place for the account's runs, and
  removes it once the login has taken — which needs the login command itself
  to have succeeded as well as the check. The token's year is counted
  from when it was stored; the month before, the harness report carries a
  warning and `yad account list` says so.
- **Every home shares the machine's config** (0054): each time a home is
  prepared, `CLAUDE.md`, `settings.json`, `skills/`, `commands/` and `agents/`
  (Claude) or `AGENTS.md` and `prompts/` (Codex) are linked from the
  harness's default home when the account has none of its own. Never a
  credential, never a file the harness keeps state in.
- **Adding and removing reach a running daemon live**
  ([0043](docs/decisions/0043-the-cli-never-writes-state-and-account-changes-reach-the-daemon-live.md)).
  The CLI never writes `state.db`: both commands change `config.toml` and send
  `accounts_changed` on the control socket, and the daemon re-reads the account
  lists — those alone — into `runner.Accounts`, the one copy every part of the
  runner reads. It checks an added account's login and writes its row; it
  forgets a removed account's rows, and no new run takes it. A run already on
  a removed account finishes there, and the daemon deletes the home when the
  last such run lets go (at once when there is none). With no daemon running
  nothing is written: `remove` deletes the home itself, and the daemon prunes
  rows for accounts `config.toml` no longer lists when it starts.
- **`config.toml` has two writers**
  ([0057](docs/decisions/0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md)):
  the CLI, and the daemon for an account a hub adds or removes. Every write —
  `yad account add` and `remove`, `yad connect`, the daemon's — goes through
  `config.Update`: an exclusive `flock` on `config.toml.lock` beside the file
  (not the file itself, which a save replaces by rename), the file read fresh,
  one thing changed, written back before the lock is let go. So neither
  overwrites the other or a hand edit made since the daemon started; the
  daemon never writes back its own copy. Comments in the file do not survive
  a write, as they did not before.
- Each harness's transcripts live once, in `<data>/transcripts/<harness>/`,
  linked into every account home (`projects/` for Claude, `sessions/` for Codex),
  so any account can resume any session. Measured on claude 2.1.278 and
  codex-cli 0.147.0 as far as the wire — a home that never created a session
  rebuilds the whole continuation from the shared directory; a provider accepting
  it under a second subscription is the part that stays untested.
  [0013](docs/decisions/0013-accounts-fail-over-and-limited-runs-wait.md) carries
  the steps and the line between the two.
- **State**: an account is `free`, `limited` until a reset, or `needs_login`
  (DOMAIN.md). It travels to every hub by label and state, in the capability
  document and in every sync's health; the home and everything the harness
  wrote in it never leave the machine.
- **Needs login**: entered three ways — a configured label whose harness home is
  not on disk, which is a label written into `config.toml` by hand; a turn
  that fails for a reason the harness does not explain when the harness's own
  login check then says the home has no login —
  never by reading the failure's wording, because Claude reports a missing
  login and a bad model identically, both inside an object that says
  `subtype: "success"`; and a hub login that ends any way but succeeded when
  that same check then says the home has no login, or cannot answer for a home
  the login itself made (`Accounts.loginNotTaken`, DEV-138) — making the home
  is what would otherwise have turned a hand-listed label free, so its
  `needs_login` is recorded before the home is made, for a daemon killed
  mid-login. A needs-login
  account is skipped for runs exactly as a limited one is, and `yad doctor`
  shows a harness whose every account needs login as `needs login`. The way back is the owner's login, by `yad account add` or by
  the harness's own command in the home: the runner re-asks the harness's login
  check every few minutes (`internal/runner.LoginProbe`) and the account is back
  in service when it answers yes. A check that cannot answer — the command
  missing, a home it could not read, a timeout — leaves the state exactly as it
  is; an unanswered question is not an answer either way. A hub can finish
  the login too — see hub login below — and add or remove an account.
- **No accounts is a state.** A harness the owner configured none for runs on
  the harness's own default home and reports no accounts. It is never an error
  that stops a runner registering. That home's login is checked as an
  account's is, at most once a minute (`capability.DefaultLogins`): without one
  the harness reports an error naming its own login command and takes no runs,
  and `yad doctor` shows it as `needs login`
  ([0053](docs/decisions/0053-a-harness-on-its-own-login-is-checked-like-an-account.md)).
- **Hub login** ([0055](docs/decisions/0055-a-hub-may-log-an-account-in-by-link-or-by-token.md)):
  any connected hub may log in an account `config.toml` lists, or a harness's
  own default login — and, with `add`, a new account, unless the owner has
  set `manage_accounts = false` on its connection (0057). `internal/runner.Logins`,
  owned by the daemon and bound to `Serve`, drives it. A login holds its
  account as a run does (`Accounts.takeForLogin`): taking the hold is the
  check that the label is still listed, made after any login it replaced has
  let go, and a removal meanwhile keeps the home until the login has stopped,
  then deletes it with whatever the login wrote there; the removal also ends
  the login `cancelled`. By link it makes the home if a hand-listed label has
  none and runs `claude auth login` there over pipes with no terminal
  (measured on 2.1.281: it prints the link and reads the code from stdin) —
  the first `https://…/oauth/authorize…` link in its output is the only thing
  read from it; the code goes to its stdin; it took only when that login
  itself exited 0 within a minute of the code and the harness's own check then
  says logged in — never by the wording, and never by the check alone, which
  says yes to a credential already in the home from before. Codex's is a
  device code over its app-server (`byDevice`, driving
  `internal/adapter/codex.DeviceLogin`, [0057](docs/decisions/0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md)):
  `account/login/start` with `chatgptDeviceCode` answers with the link and the
  code, reported as `url` and `user_code`; `account/login/completed` with
  `success` is the login's own condition, and once the app-server has exited
  on its input closing — after writing `auth.json` — `codex login status`
  decides. A login ended from the hub, or unentered after ten minutes, sends
  `account/login/cancel` before the app-server is stopped, so a code typed
  late logs nothing in. What Codex words — an error, a refusal — is never
  read or reported; the runner writes its own. That behaviour is read from
  0.147.0's schema, not yet measured against a real login. A stored token stays in its file for the
  account's runs throughout: the login runs with `account.LoginEnv` and is
  judged by `account.OwnLogin`, both without the token — claude's check says
  yes to any token — and the token is removed only once the login has taken,
  as a plain `yad account add` on a token account does. By token it is
  `account.SetToken` and `account.LoggedIn`. On a yes the account goes free
  through `Accounts.LoggedInAgain`, the half of an `accounts_changed` reload
  that checks an added account, and the capability document is rebuilt at
  once; a default login drops the minute-long cached answer instead. Thirty
  seconds for the link, ten minutes for the code (`expired`), a minute to
  exit; one login per account, a newer one superseding. A token for Codex or
  with no account, and an unlisted account, end `failed` with the command that
  does it at the machine.
  An add holds a label nothing lists yet (`Accounts.takeForAdd`, which also
  keeps a home a removal left waiting to be deleted), and is refused before
  anything is touched for a label already listed, one `config.ValidName`
  refuses, none, or a hub not allowed to add. On a yes it goes through
  `Accounts.Add` — the label into `config.toml` under its lock, then the
  lists reloaded as `accounts_changed` reloads them — instead of
  `LoggedInAgain`; on anything else nothing is listed and the home is kept
  for a retry. `remove_account` goes through `Accounts.Remove`, the same
  removal as `accounts_changed`'s after the same locked write; a label
  neither the file nor the lists name is nothing to do, and one from a hub
  not allowed to is ignored. Logins are memory only — nothing in `state.db`, the code and
  the token in no log, report or error — so a restart forgets those in flight.
  `yad hub` keeps them in `hub.db` and blanks the token once the runner has
  reported its login, and the code once it has taken it. It ends a login on
  its own word only while no answer has carried it (`sent_at`); once one has,
  a newer login or a cancel sends `cancel_login` and waits for the runner's
  report, and an end the hub did write gives way to the runner's report of
  one. A sent login unheard for thirty minutes (`loginFinishWithin`) ends
  `failed`, which blanks its token.
- **Detection**: Codex publishes `account/rateLimits/updated` with each window's
  use and reset; Claude reports a limit in its result with a reset time, and
  carries every window's use and reset in each `rate_limit_event`
  (`unifiedWindows`), including the ones that say the turn was allowed.
- **Never a rate limit.** Transient API throttling the harness retries by
  itself — Claude's `system/api_retry`, Codex's `willRetry` error — is a rate
  limit, not a usage limit (DOMAIN.md). It is counted as `api_retries` in the
  result's metrics and changes no account's state. A 429 that survived every
  one of the harness's own retries and ended the turn is an exhausted account
  and is treated as a usage limit. It carries no reset, so one is taken from
  the soonest window the harness called full and still to reset
  (`internal/account.RefillAt`) — a full window whose reset has already passed
  is the residue of a limit already over and says nothing about this one — and
  only failing that from a short constant
  (`internal/account.limitWithoutReset`), which also covers a reset the harness
  itself gave in the past. An undated limit is a park nothing ends, and a long
  guess idles an account the owner pays for.
- **The reset is the authority.** An account's `limited_until` decides whether
  it is limited; the stored `state` is derived from it at every read, so a
  limit that has passed needs no writer to come along and clear it
  (`internal/account.stateOf`). "Now" is always the caller's: `account.Load`
  and `account.Read` take the moment they judge against and read no clock of
  their own, the sync loop passes its injected `Clock`, and a report never
  re-judges what Load decided — so a test on a fake clock sees limits on that
  clock rather than on the wall (DEV-85).
- **On a limit**: mark the account limited until its reset → the free account
  whose window resets soonest ([0039](docs/decisions/0039-accounts-log-in-themselves-and-the-soonest-reset-goes-first.md)) → resume the same session with a continuation turn, which is the
  run's own instruction again against the session's native id: the transcript
  holds the turn that was cut short, and nothing is invented to prompt with.
  Each move costs one cache-cold turn
  ([0013](docs/decisions/0013-accounts-fail-over-and-limited-runs-wait.md)), which
  is why the soonest-resetting account is preferred and stayed on rather than
  ping-ponged between. `account_switches` counts the moves — including one made
  across a park, since the run reads the account it last ran on back from its
  row. A run is **one run** however many accounts and however many processes it
  took: `usage`, `tool_calls`, `api_retries` and `stalls` cover every turn, and
  `wall_clock_ms` is one budget spent across all of them rather than handed out
  afresh per account — a park spends none of that budget, since `max_wait_ms`
  is what caps a wait and two names for one limit would leave one of them
  meaningless. `first_event_ms` is the one metric that is **not** the whole
  run: it is the answering turn's own offset, because a run parked five hours
  and then answering in two seconds is not a harness that took five hours to
  speak, and `waited_ms` already reports the park.
- **Choosing among free accounts**: soonest refill first, counting only windows
  the account has spent something of — an untouched window has no quota to waste
  (`internal/account.Soonest`). The owner's list names which accounts take part
  and breaks ties; it is not a priority order.
- **None free** → the run becomes **waiting** with `resumes_at`, the earliest
  reset among that harness's limited accounts. It holds no process **and no
  goroutine**: the run's goroutine ends, its capacity and any path-source lock go
  back, and everything it had is in its row — when it first started, the wait it
  has served, the accounts it has been through, and what its turns so far cost.

  **It comes back through its own connection's sync loop, and through nothing
  else.** A claim is an answer to a sync; so is a resume. Running it inside the
  sync is what makes it impossible for a resumed run to precede a sync, to run
  before `Recover` or during a drain, to take capacity outside the sync's own
  reservation, or to *start* on a connection with no reporter to deliver its
  result — not because each is checked, but because there is no code path on
  which they are false. (It may well outlive its loop, and is meant to:
  `runsOn` starts every run on the server's context so a connection that stops
  leaves the runs in hand to finish.) It was a sweep of its own, and four
  review rounds each found one clause of "may I start work now?" that the sweep
  had not restated; a predicate copied by hand drifts from the original.

  The cost is one sync interval — 15 s by default, bounded 5–60 s, chosen by the
  hub — **provided the run's capacity is held before the hub is asked**, which
  it is: a sync takes a unit for each parked run that is due and advertises what
  is left, so the hub cannot fill that unit with a fresh offer. Without that the
  cost is not one interval but unbounded: a hub fills whatever capacity the
  request advertises, and those offers are claimed before the resume is reached,
  so on a runner with a standing queue the parked run never moves.

  A restart is therefore not a second path but the same one, which is why
  `kill -9` costs a waiting run nothing and why
  [0030](docs/decisions/0030-a-restart-reports-lost-and-replays-first.md)'s
  report-lost pass skips it. The sync also decides when the run is due: the
  resume time is a floor, and an account freed before it — the owner finishing a
  login, which `internal/runner.LoginProbe` notices — brings the run back at the
  next sync, which is the "account frees" half of §2's arrow. Within five seconds
  of the run's own reset nothing counts as early: `limited_until` passing and the
  run becoming due are one moment seen through two clocks, and acting on it buys
  a cache-cold turn against a limit that has not really lifted.

  The one thing the row cannot hold is the run's grants — they live in the
  process that claimed them and never touch disk — so a waiting run that had them
  and is picked up by a *later* process is reported `lost` with class
  `grants_lost` for the hub to send again. A hub's `max_wait_ms` caps the total
  wait, after which the run is `timed_out` with class `max_wait_exceeded`; the
  collector ends such a run for a connection no loop is serving, which is the
  only thing in the process that touches a parked run without a sync, and it can
  only ever end one. A cancel for a waiting run has no turn to interrupt and ends
  it where it stands, and so does an interrupt.
- **While every account of a harness is limited or needs login**, the runner
  stops claiming for it: health says `ready: false` and the offer is left
  unclaimed for the hub to place elsewhere, rather than refused — a refusal is
  terminal, and there is nothing wrong with the run.

## §4 Local state and configuration

### Profiles and paths

| | default profile | `--profile work` |
|---|---|---|
| config | `~/.config/yad/` | `~/.config/yad/profiles/work/` |
| data | `~/.local/share/yad/` | `~/.local/share/yad/profiles/work/` |

`$YAD_CONFIG_DIR` and `$YAD_DATA_DIR` override both. The config directory holds
`config.toml`, `runner-id` and `credentials/<connection>` (each `0600`); the
data directory holds `state.db`, `workdirs/`, `repos/`, `accounts/`,
`host-tools/` (0045),
`transcripts/`, `logs/` (the daemon's JSON log `yad.log`, rotated at 10 MiB into
`yad.log.1`–`.3`, and `stderr.log` for what a background start printed before
its log was open), the control socket `yad.sock` and the daemon lock `yad.lock`
([0026](docs/decisions/0026-the-daemon-lock-is-a-held-flock-beside-the-socket.md)) — and `hub.db` when
the machine also runs `yad hub`. A machine that submits to a hub keeps its
admin token in the config directory's `hub-admin-token` (`0600`).

### `config.toml`

```toml
name     = "ashikaga"
labels   = ["macos", "home"]
capacity = 4

[harness.claude]
permission_mode = "bypassPermissions"   # the owner's call — 0015; the default when unset
cap             = 3
accounts        = ["personal", "family"] # which take part; the order breaks ties (0039)

[harness.codex]
sandbox  = "danger-full-access"   # the owner's call — 0036; this and never are the defaults when unset
approval = "never"
cap      = 2
accounts = ["personal"]

[[connection]]
name = "yashiki"
url  = "https://ashikaga.tail.ts.net/yad/v1"
cap  = 2   # at most this many runs held for this hub at once; absent = only capacity limits it
manage_accounts = false   # this hub may not add or remove accounts; absent = it may — 0057. Read at start

[sessions]
idle_ttl   = "336h"   # close sessions idle this long; "0s" keeps them — 0035
disk_floor = "5GiB"   # below this free under the workdirs, idle sessions go; "0" is off

[drain]
wait = "30m"   # how long a drain lets runs finish before cancelling them — 0029

[workdirs]
roots         = ["/home/me/src"]  # where path sources and local git URLs may point — 0033; unset = your home directory (0038)
git_timeout   = "10m"
setup_timeout = "15m"
```

### `state.db`

SQLite (WAL, `0600`) via `modernc.org/sqlite`; queries in `internal/store/*.sql`,
Go generated by `sqlc` and committed. Tables: `sessions`, `runs`, `events`
(the spool — unique `(connection, run_id, seq)`), `outbox`, `accounts` (each
account's state, and its reset when limited), `slots`; a session row keeps the sources its workdir was built from. Session and run ids are the hubs', so every one is keyed with
its connection: two hubs may pick the same id. The schema version is `PRAGMA
user_version`; migrations are embedded and run on open. The daemon is its only
writer: the CLI opens it read-only, and a change the CLI makes goes through the
control socket
([0035](docs/decisions/0035-a-runner-reports-every-close-in-its-sync.md),
[0043](docs/decisions/0043-the-cli-never-writes-state-and-account-changes-reach-the-daemon-live.md)).

### `hub.db`

`yad hub`'s own store, a separate SQLite file opened the same way, with its
queries in `internal/hub/store/*.sql`. Tables: `registration_tokens`,
`runners` and `admin_tokens` (secrets only as SHA-256 hashes), `sessions` (the runner each is bound
to), `runs`, `events` (unique `(run_id, seq)`) and `results` (one per run). A
run's hub-side state adds two before the protocol's: `queued` and `offered`.
A run's spec holds its grants' values only until the run reaches a terminal
state: the `runs_forget_grants` trigger blanks them then and keeps their names
([0041](docs/decisions/0041-a-hub-holds-a-grant-only-while-its-run-can-use-it.md)).
`logins` holds each hub login ([0055](docs/decisions/0055-a-hub-may-log-an-account-in-by-link-or-by-token.md)),
`requested` until its runner first reports it, with `sent_at` once an answer
has carried it and `hub_ended` for an end the hub wrote itself; the
`logins_forget_token` and
`logins_forget_code` triggers blank a token once the runner has reported its
login and a code once the login has moved past `waiting`, whoever moves it.
A login's `add_account` marks one that creates its account, and
`account_removals` holds each removal asked for until neither the runner's
capability document nor its health lists the account
([0057](docs/decisions/0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md)).
Both databases are opened with `secure_delete`, so freed bytes are zeroed
rather than left in the file.

## §5 The local surface

```
yad doctor                         what is installed, what YAD can drive, and what about
                                   this machine or profile is reachable by other users
yad harnesses [--json]             the capability document, as a hub receives it but for
                                   the accounts feature, added per connection (0057)
yad connect <url> --token T|-      register with a hub (- reads the token from stdin — 0020)
yad disconnect <name>              not built: its coordination with a running
                                   daemon is being designed (DEV-81); it refuses
                                   and says so
yad daemon start|stop|restart|status|logs [-f] [-n N]
                                   the runner process
yad status [--json]                connections, capacity, runs, sessions and recent
                                   errors — via the socket
yad sessions [--json]              the sessions held: workdir, runs, last use — read
                                   from state.db read-only, so the daemon may be down
yad sessions close [--connection c] <id>
                                   close a session and reclaim its workdir, via the
                                   daemon; one with a run held closes when it ends
yad account add <harness> <label> [--token - | --device]
                                   run the harness's own login in that account's
                                   home, with the owner there (--device: Codex's
                                   device code); or, for Claude, store a
                                   `claude setup-token` token read from stdin
                                   (0054). Added only once logged in; a running
                                   daemon takes it up at once, via the socket (0043)
yad account list [--json]          the accounts held, and each one's state
yad account remove <harness> <label> [--yes]
                                   delete that account's home and its login; the
                                   shared transcripts are kept. A running daemon
                                   stops using it at once, and a run on it
                                   finishes there before the home goes
yad account use                    not built, and not planned: it refuses, saying a
                                   run takes the free account that refills soonest
                                   (0039), so there is nothing to pick by hand
yad service install|uninstall|status
                                   launchd user agent, systemd user unit (0028)
yad hub serve                      the standalone hub: protocol at /v1, service API at /api/v1
yad hub submit --harness h --model m [--effort level] [--session id | --new-session id] <instruction | ->
                                   queue a run; prints its id (--watch follows it)
yad hub watch <run>                a run's events as they arrive, then its result;
                                   exits non-zero unless it succeeded
yad hub cancel <run>               stop a run: at once when no runner started it,
                                   else by its runner, down the cancel ladder
yad hub interrupt <run>            end a run's turn, keep its session
yad hub steer <run> <text | ->     add input to a running turn
yad hub runners [runner] [--json]  the runners this hub knows and the health each
                                   last reported: why one is slow or idle
yad hub drain <runner>             the runner takes no new runs, finishes those it
                                   holds and exits
yad hub close-session <session>    the session takes no new run; its runner deletes
                                   its workdir
yad hub login start <runner> <harness> [account] [--add] [--code -]
                                   log an account in on a runner by link: prints the
                                   link once the runner has it, reads the code on stdin
                                   (Codex: prints the link and its device code, reads
                                   nothing, and waits for the owner to type it);
                                   --add creates the account
yad hub login token <runner> <harness> <account> [--add]
                                   log it in with a setup-token token read from stdin
yad hub login status|cancel <runner> <login>
yad hub account remove <runner> <harness> <account>
                                   the runner removes the account; waits until it has
yad hub admin-token create|list|revoke
                                   the service API's tokens; create saves to a 0600
                                   file and prints nothing secret (--out - prints once)
yad hub token create [--ttl 1h] [--runner id]
                                   a one-time registration token; --runner re-registers
                                   that runner, the only way to replace its credential
yad conformance <url> --token T [--second-token T2]
                                   check any hub against v1: every rule it
                                   breaks, where that rule is written, and what
                                   it does not check; the second token registers
                                   a runner that must not be able to report on
                                   the first one's run
yad upgrade [--check] [--force] [--tag v]
                                   replace this binary with the newest release
```

`yad daemon start` backgrounds itself — it re-executes `yad daemon start
--foreground` in a session of its own and returns once that process answers on
its socket; `--foreground` is what service units run. Logs are JSON through
`log/slog`, rotated by size; a foreground daemon whose stdout is a file or pipe
(a service unit's `service.log`) writes nothing more there once that log is
open. The control socket is `0600` in a data directory
that must itself be private, one JSON request and answer per connection — but
a stop, which the daemon acknowledges and acts on only once the CLI confirms
it ([0027](docs/decisions/0027-stop-asks-then-signals-and-restart-checks-first.md));
its protocol is internal and unversioned. The daemon holds `yad.lock` with
`flock(2)` for its life, which is the single-instance lock per profile: a
socket file a crash left behind never blocks a start
([0026](docs/decisions/0026-the-daemon-lock-is-a-held-flock-beside-the-socket.md)).
A socket path past the kernel's limit (103 bytes on macOS) is refused with the
fix, never moved elsewhere. `yad daemon stop` asks for a graceful stop through
the socket — the drain a first stop signal starts
([0029](docs/decisions/0029-drain-is-a-three-signal-ladder.md)): a second
signal cancels the runs held, a third exits at once — and falls back to
signals; `restart` checks every credential locally
before it stops anything
([0027](docs/decisions/0027-stop-asks-then-signals-and-restart-checks-first.md)).

## §6 Dependencies

Curated, per [0016](docs/decisions/0016-a-curated-set-of-dependencies.md). A new
line here is a reviewed change.

| | Why |
|---|---|
| `modernc.org/sqlite` | durable ordered state without cgo, so cross-compiles stay one command |
| `github.com/pelletier/go-toml/v2` | the owner edits `config.toml` by hand |
| `github.com/danielgtaylor/huma/v2` | typed operations for `yad hub`, and the OpenAPI document they generate |
| `sqlc` (tool, not linked) | typed queries from SQL |
| `staticcheck` (tool, not linked) | the lint bar in `CHECKS.md` |
| `oasdiff` (tool, not linked) | the breaking-change check on both OpenAPI documents ([0017](docs/decisions/0017-protocol-types-are-the-source.md), [0022](docs/decisions/0022-hub-service-api-beside-the-protocol.md)). Pinned rather than installed per machine because a gate is worth only as much as its verdict is reproducible, and two boxes answering differently is the failure. v1.32.1 brings roughly thirty indirect modules — cobra, viper, afero and the rest of a CLI's furniture — into `go.sum`; none is linked into `yad`, and that price is named here rather than glossed |
| `govulncheck` (tool, not linked, not in `go.mod`) | CI's check for known vulnerabilities in the code `yad` actually calls, on every push and weekly. Run as `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0` so it adds nothing to `go.sum`: it is the one check whose answer changes without a commit, and it runs where the vulnerability database is fetched |

## §7 Testing

- **No test spends a token or touches the network.** Harness and host-tool
  detection run against an empty `PATH`; adapters replay fixtures. An empty
  `PATH` is not the whole of it: a `YAD_*_PATH` override is consulted *before*
  `PATH`, so a test that must reach no real binary clears those too. It passes
  either way on a machine where none is set, which is what makes forgetting
  invisible.
- **Two fakes.** `internal/adapter/fake` plays a scripted run in memory, for
  runner and hub logic. Child-process behaviour — hangs, ignored `SIGTERM`,
  oversized lines, orphaned grandchildren — is tested by re-executing the test
  binary as the child (`SUPERVISE_TEST_CHILD=<mode>`). The Claude adapter's
  tests re-execute it as a fake `claude` that plays a recorded stream and reads
  stdin as Claude does (`CLAUDE_TEST_FIXTURE=<file>`); the Codex adapter's, as
  a fake `codex app-server` that answers each request the recording answered
  (`internal/adapter/codex/codextest`, `CODEX_TEST_FIXTURE=<file>`).
  Re-executed children set `GORACE=atexit_sleep_ms=0`, or each costs a second.
- **The runner is tested against `yad hub`**, in process, on a random port, and
  **`yad hub` is tested against the conformance suite** — `internal/conformance`,
  which holds a URL and a registration token and nothing else. It imports no
  package of the hub's, so the checks that pass `yad hub` on a local listener
  (`internal/hub/conformance_test.go`) are the same ones a TypeScript hub is
  judged by. The suite's own tests run it against a fake hub in its own package:
  one that follows §2, and one per rule that breaks exactly that rule, because a
  failure is only worth what it says to whoever has to fix it.
- **End to end** (`cmd/yad/e2e_test.go`): the commands an operator types —
  token, connect, daemon, submit, watch, cancel, interrupt — against `yad hub`
  in process, with a real adapter driving the test binary as its harness. Every
  path runs once per harness (`e2e_harness_test.go`): the Claude adapter
  against a fake `claude`, the Codex adapter against the fake `codex
  app-server`, which for these tests also keeps a rollout per thread as codex
  does, so a resume finds the thread before it or is refused. A run succeeds; the network drops mid-run and every event and the
  result still land; the runner restarts mid-run and the run is reported lost,
  with its events delivered, and the next run resumes its session in the same
  workdir; two runs in one session, the second answering from the first's
  context, then a resume whose transcript is gone failing `resume_rejected`; a
  run cancelled or interrupted mid-run ends cancelled, with its
  latency measured and no process left; a runner process gets one, two and
  three real stop signals and drains, cancels, or exits; `yad hub drain`
  drains a runner, which exits by itself; a run with a git source — a bare
  repository on disk inside the owner's root — is checked out, its setup hook
  writes a file, and the harness answers with what it read there. One runner
  with both harnesses drives a run of each at once. And Codex's recorded
  resume, missing rollout and interrupt play as recorded
  (`e2e_codex_test.go`).
  `internal/workdir`'s tests use local bare repositories, and a loopback TLS
  server for a remote that asks for a password or never answers.
- **Real harnesses** only behind `//go:build realharness` and
  `YAD_REAL_HARNESS=1`, run by hand — and `make smoke` and `make
  smoke-codex`, the same path as the end-to-end tests with the real `claude`
  or `codex`, the built binary and `yad hub serve` (`scripts/smoke.sh
  <harness>`; a few cents of haiku or gpt-5.6-luna, `SMOKE_MODEL` to change
  it).
- **No release is downloaded.** `internal/upgrade` fakes the release source
  outright, and reads what the installed binary held *at the moment the
  download ran* to prove nothing was replaced before the checksum was checked.
  `gh` itself, and `scripts/install.sh` around it, are tested against a `gh`
  that is a shell script on `PATH` — which proves the argv, the checksum gate
  and where the binary lands, and proves nothing about a real GitHub release.
- **A future time is computed, never written.** A test that needs one takes it
  from the clock — `time.Now().Add(...)` or an injected clock — because a
  literal future date silently changes the test's meaning the day it passes,
  and stays green while it does. A literal instant is safe only as a past fact,
  or as a value nothing compares to now.
- Table-driven, `t.Setenv`, `t.TempDir`, no assertion library; `-race` always.

## §8 Security

- The runner runs as an ordinary user, never root; the service units say so.
- Tokens: `0600` files, never logged, never printed, never in argv, never in an
  event. Grants are deleted when their run ends — including by the next start,
  after a crash that skipped the deletion. A grant file that cannot be unlinked
  is overwritten and truncated instead, and what survives both is logged by
  directory, never by name. An `env` grant is `NAME=value`
  in the harness's environment; a `file` grant is a `0600` file whose path is
  in `NAME`, in a directory of the run's own under `<data>/grants` — never in
  the checkout the harness works in, though a run whose `path` source is the
  owner's home directory has the data directory somewhere beneath it. A grant's
  name is any valid environment variable name
  (`[A-Za-z_][A-Za-z0-9_]*`, which is also a plain file name) except four that
  would break the run rather than attack it: `PATH`, `HOME`, `LD_*` and
  `DYLD_*`, matched whatever their case — and the variables that choose whose
  credential a harness uses or which home it logs in from
  (`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`,
  `ANTHROPIC_PROFILE`, the federation pair, `ANTHROPIC_CONFIG_DIR`,
  `CLAUDE_CONFIG_DIR`, `ANTHROPIC_BASE_URL`, `ANTHROPIC_CUSTOM_HEADERS`, every
  `CLAUDE_CODE_USE_*` provider switch, `CODEX_HOME`, `OPENAI_API_KEY`,
  `CODEX_API_KEY`, `CODEX_ACCESS_TOKEN`, `OPENAI_BASE_URL`,
  `CODEX_REFRESH_TOKEN_URL_OVERRIDE`, `AWS_BEARER_TOKEN_BEDROCK` — `accountGrantNames` in
  `protocol/v1/grant.go` is the list itself), because a grant is appended after
  `supervise.Scrub` and would put the turn on a credential the run's account
  knows nothing about while its events named the account
  ([0040](docs/decisions/0040-a-grant-may-not-move-a-run-off-its-account.md)).
  Two `file` grants whose names differ
  only by case are refused too: on a case-folding filesystem they are one file,
  written in order, so the second truncates the first and both names end up
  pointing at the second grant's value. `protocol/v1`
  checks it; a run carrying one that fails is refused by the hub and by the
  runner, never run with it
  stripped — [0038](docs/decisions/0038-the-owner-trusts-the-hubs-it-connects.md),
  which supersedes 0024's secret-shaped suffix and its reserved namespaces.
  A name the owner rather than the hub decides keeps its own guard, which is
  not a naming rule: `IS_SANDBOX` is an acceptable grant name and the Claude
  adapter still strips it from the run's environment (0015).
  Filtering names never protected the machine: the brief could ask the harness
  for the same secret, and it auto-approves. The service API never returns a
  grant.
- Three secret kinds, never interchangeable: registration token, runner
  credential, admin token. The protocol accepts only the first two, the service
  API only the third.
- Permission mode and sandbox are runner configuration per harness; no protocol
  field can set them — [0015](docs/decisions/0015-owner-environment-is-the-trust-boundary.md).
- The owner trusts the hubs it connects, so YAD does not police what a hub
  sends ([0038](docs/decisions/0038-the-owner-trusts-the-hubs-it-connects.md)).
  Hub input and harness output are still data to YAD itself: never executed,
  never passed to a shell, never an instruction to the runner.
- A run's sources are argv, never a shell: https and ssh only, no remote
  helpers, no leading `-`, no password in a URL, and nothing on the machine
  outside the owner's `[workdirs] roots`, which default to the owner's home
  directory when unset —
  [0033](docs/decisions/0033-sources-reach-only-what-the-owner-allows.md),
  [0038](docs/decisions/0038-the-owner-trusts-the-hubs-it-connects.md). Those
  guards prevent bugs, not attacks: the trust boundary is the machine and its
  owner.
- `claude -p` loads a repository's `.claude/settings.json` hooks and `.mcp.json`
  servers without a trust prompt. With auto-approve that is no worse than the run
  itself — which is exactly why a runner belongs on a machine you would let the
  repository run code on.
- `yad doctor` warns on the three exposures an owner can fix: running as root,
  a config or data directory another user can reach — by its mode, or by owning
  it, the same pair `control.checkDir` refuses before it binds the socket — and
  a profile file another user can read (`config.Exposures`). The files that hold
  a secret say how to retire it as well as how to close it: a chmod stops the
  next reader and not the one who already read it. They are warnings and never change
  its exit code — absence and misconfiguration are both facts it reports, and a
  diagnostic that refused to run would answer a question nobody asked. Some of
  what it reports is refused elsewhere and some is not: `config.ReadSecret`
  refuses an exposed credential or admin token, and `control.checkDir` refuses
  an exposed **data** directory before binding the socket. Nothing refuses an
  exposed `state.db`, `hub.db` or `config.toml`, and nothing else looks at the
  config directory — which is why doctor is where an owner hears about those.
- The operator-facing version of this section is
  [docs/run-it-safely.md](docs/run-it-safely.md).

## §9 Build order

The epics live in Zumino (`yad/dev`), as vertical slices: each ends in a runner
that does something real end to end, and each is thin across every layer rather
than complete in one.

| | |
|---|---|
| **E1** | foundation — this model, this file, and a base that builds and tests |
| **E2** | one Claude run, end to end, through `yad hub` — the whole path, thinnest |
| **E3** | the runner for real — daemon, control socket, service units, drain, restart safety |
| **E4** | sessions and workdirs — resume, git sources, setup hook, slots, GC |
| **E5** | Codex — the second adapter proves the interface |
| **E6** | accounts and usage limits — failover, waiting, restart survival |
| **E7** | many hubs — shared capacity, caps, grants, host tools, conformance suite |
| **E8** | operating it — health, metrics, versioning, packaging |
| **E9** | later — the backlog: self-update, live sessions, release, ACP, … |

## §10 What Multica taught

Read for shapes, never copied — [0014](docs/decisions/0014-multica-shapes-never-code.md).
Upstream `multica-ai/multica@2df765a`, read 2026-09-18. The lessons this design
already encodes, so they are not learned twice:

- A claimed run must never wait for capacity: take the slot, then ask.
- Pin the native session id mid-run; a crash otherwise loses the resume pointer.
- The terminal report needs a durable outbox; events need a unique `(run, seq)`
  — Multica admits it has no dedupe.
- Exit 0 is not success; `prompt_too_long` arrives as `success`; an echoed session
  id that differs means the resume failed.
- `--version` can hang, and hung every registration once: probe with a timeout.
- Write the prompt to stdin from its own goroutine, or it deadlocks against the
  banner; close stdout before `Wait` after a scanner error.
- Codex multiplexes subagent threads on one pipe, and a resume replays history as
  notifications: filter by thread, gate on the current turn.
- A 10 MiB line cap made an oversized thread unresumable forever; 32 MiB.
- Secrets in argv are visible to `ps`.
- A daemon started from a GUI has no shell `PATH`.
- Restart must check the credential before stopping the old process.
- The idle watchdog grew from 5 minutes to 2 hours; start generous.
