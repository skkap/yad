# Architecture

What YAD is made of, what it says on the wire, and how it drives a harness.
[`DOMAIN.md`](DOMAIN.md) owns the words; this file owns the shape; the *why* of
every choice below that was hard to reverse is a record in
[`docs/decisions/`](docs/decisions/). Where a section and a record disagree, the
record is older and this file is stale.

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
internal/hostool         probing host tools (gh, git, docker, zumino)
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
internal/conformance     the protocol conformance suite, run against any hub
```

Dependencies point downward only: `cmd` → `runner`/`hub` → everything else;
`adapter/*` never import `runner`; `protocol/v1` imports nothing of ours.

## §2 The protocol — v1

JSON over HTTPS. The Go types in `protocol/v1` are the source; `openapi.yaml`
beside them is generated and committed, and a test fails when they drift —
[0017](docs/decisions/0017-protocol-types-are-the-source.md). Nothing in the
protocol is harness-specific: a hub never learns what a rollout file is.

A **connection** is a base URL — `https://zumino.cc/api/yad/v1` — and every path
below is relative to it, so a hub can mount the protocol anywhere.

### Calls

| | |
|---|---|
| `POST /runners/register` | registration token → runner credential; full capability document in, hub features and timings out |
| `POST /runners/{runner}/sync` | the periodic call: state and health in; runs, control messages and the next interval out |
| `POST /runs/{run}/events` | a batch of events, idempotent by `(run, seq)`; answers `acked_through` |
| `POST /runs/{run}/result` | the terminal state, idempotent; retried from the outbox until acknowledged |
| `POST /runners/{runner}/deregister` | the credential dies; the hub marks held runs lost |

Every request carries `Authorization: Bearer <runner credential>` (the
registration token, for `register` only), `Yad-Protocol: 1` and
`User-Agent: yad/<version>`. Errors are `{"error": {"code", "message",
"next_action"}}` — the next action is mandatory, because a runner on a
customer's machine is debugged by reading it.

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

```
→ { runner_id, fingerprint, capabilities?,        // document only when asked
    health: { load, free_capacity: {total, by_harness}, disk_free_bytes,
              harnesses: [{id, ready, accounts: [{label, limited_until?}]}],
              spool_depth, outbox_depth, recent_errors[], draining? },
    runs: [{ run_id, state, resumes_at?, reason? }],    // every run held
    closed_sessions: [{ session_id, reason, closed_at }] }  // until answered
← { next_sync_ms, lease_ms,
    runs: [Run],                                  // never more than free capacity
    controls: [{ kind, run_id?, session_id?, text? }],
    min_version? }
```

- **Control kinds**: `cancel`, `interrupt`, `steer`, `close_session`, `drain`,
  `report_capabilities`, `update` (reserved —
  [0018](docs/decisions/0018-no-self-update-in-v1.md)).
- **Controls are not acknowledged**, so a hub repeats `cancel` and `interrupt`
  in every response to a sync listing the run, until the run ends; the runner
  acts on the first. A `steer` is sent once — twice, the harness would read it
  twice. An interrupt that reaches a run before its harness is up ends it as a
  cancel does, with nothing spawned —
  [0025](docs/decisions/0025-a-cancel-is-repeated-and-an-answer-that-landed-stands.md).
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
- **Claim by listing.** A run offered in a sync response is claimed when the
  runner lists it in its next sync. An offered run that the next sync does not
  list was never received, and the hub offers it again. The runner takes the
  capacity *before* it syncs, so it can always start what it is offered.
- **Start on acknowledgement** — [0019](docs/decisions/0019-a-run-starts-once-its-claim-is-acknowledged.md).
  The runner starts a run only after a sync listing it has been answered without
  a `cancel` for it, and syncs again at once when offers arrive, so a run starts
  one round trip after it is offered. A listed run the runner does not hold is
  answered with `cancel`. A run the runner will not take is reported as a
  `failed` result with error class `refused`.
- **Sessions stay put.** The first claim in a session binds it to that runner;
  its later runs are offered to that runner alone, one at a time.
- **Lease.** Every sync renews the lease on every run it lists. A run whose lease
  lapses (default: four missed intervals) is **lost** on the hub's side.
- **Timings belong to the hub**: default interval 15 s, bounded 5–60 s; the
  runner adds ±10 % jitter and backs off 1 s → 30 s on errors.

### Run

```
{ run_id, session: { id, new, mode: "per_run" },
  harness, model,
  brief: { context, instruction },
  sources: [{ git: { url, base, branch } } | { path }],
  grants: [{ name, value, as: "env" | "file" }],
  start_at?, max_wait_ms?, wall_clock_ms?, inactivity_ms? }
```

`session.mode = "live"` is reserved and refused until a runner advertises it.

### Run states

```
claimed ─► preparing ─► running ─► succeeded | failed | cancelled | timed_out
                          │  ▲
                          ▼  │ (limit resets / account frees)
                        waiting ──────────────────────────► timed_out (max_wait)
   any non-terminal state ── runner lost ──► lost      (decided by the hub)
```

`preparing` covers the workdir, the setup hook and the account; a run reports
`running` only once its workdir exists (Multica #3999). Non-terminal states
travel in syncs; the terminal one travels in the result. A finished run whose
result is not yet acknowledged stays listed, as `running`, so its lease outlasts
a hub outage — [0023](docs/decisions/0023-lost-stands-against-a-late-result.md).

### Events

```
{ seq, at, kind, text?, tool?: { id, name, input?, output?, truncated? },
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
after it ends; anyone else gets `403 not_holder`.

### Result

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

The major version is in the path. Within it, both sides advertise feature
strings — the runner in its capability document, the hub in its register
response — and nothing is used that the other side did not advertise: a hub
sends `drain`, `close_session`, `steer` and `interrupt` only to a runner
advertising each, and offers a run carrying `start_at` only to a runner that
will hold it back rather than start it at once. `yad hub` advertises no
`hub_features` of its own — it has nothing beyond the v1 baseline.

A hub may refuse a runner below `min_version` with `version_too_old` and a next
action. `yad hub serve --min-version 0.4.0` sets that floor: a runner under it
is refused at register — before its registration token is burned, so the
upgraded runner can still use it — and at every sync, with the next action
`yad upgrade`. The floor rides in the register and sync responses as
`min_version`, so a runner can say what it is being asked for. Versions are
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
| `POST /runners/{runner}/drain` | a `drain` control, repeated until the runner says it is draining; 409 for a runner without the `drain` feature |
| `GET /sessions/{session}` | the session: its runner, and `open`, `closing` or `closed` with the reason |
| `POST /sessions/{session}/close` | a session no runner holds closes here, its unstarted runs cancelled; a held one gets `close_session` until its runner reports it closed; 409 for a runner without the `close_session` feature. A closing or closed session takes no new run |

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
       --disallowed-tools AskUserQuestion --permission-mode <owner config>
       (--session-id <uuid> | --resume <uuid>) [--model m]
       [--append-system-prompt-file <context file>]
```

The instruction is one stream-json `user` frame on stdin, written from its own
goroutine; stdin stays open for `control_request` (interrupt) and steers until
the last `result`, and closing it is what lets Claude exit. YAD chooses the
session id, so nothing has to be scraped; an echoed id that differs means the
resume silently failed, and the run fails with `session_mismatch`. A resume
Claude refuses — no transcript for the id — fails with `resume_rejected`
([0031](docs/decisions/0031-a-failed-resume-is-the-hubs-to-decide.md)).
`AskUserQuestion` is disallowed — headless, it returns an empty answer. A steer
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
with the instruction; the brief's context is the thread's
`developerInstructions`. Each of those is answered within 30 s. The thread id is
the native session id, exposed the moment `thread/start` answers. Codex writes
subagents' threads to the same pipe and a resume replays the thread's history,
so only notifications naming the run's thread and, once it has started, the
run's own turn are read. Only `turn/completed` decides the run; a steer is
`turn/steer` into the same turn, an interrupt `turn/interrupt`; once the turn is
over input closes and the app-server gets 2 s to exit. A resume Codex has no
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
  anything `YAD_*`), and a stderr tail kept at 2 KiB. git and setup hooks start
  with `NoTTY` — a session of their own, no controlling terminal — so nothing
  they run can prompt. `Start` hands back the raw
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
  claim it never began is withdrawn, for the hub to offer again. The
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
  source must resolve inside the owner's `[workdirs] roots` — none configured,
  none taken
  ([0033](docs/decisions/0033-sources-reach-only-what-the-owner-allows.md)).
  `internal/workdir` does all of it; the executor calls `Prepare` on the
  session's directory in `preparing`, spools what it reports as the run's
  first events, and fails the run with `source_refused`, `source_failed` or
  `setup_failed`.
- **Setup hook**: if the worktree has an executable `.worktree/setup`, it runs
  with `WT_ROOT`, `WT_MAIN` (the bare cache), `WT_BRANCH`, `WT_SLUG`, `WT_REPO`
  and `WT_SLOT` — the contract in `~/my/gpi-tools/docs/worktrees/README.md` —
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
  waiting ones included: its close waits for the run to end. The close is
  recorded first and the workdir removed by the collector's sweep —
  `workdir.Manager.Reclaim` first takes its worktrees out of their bare caches
  and frees its slots — which retries a removal that failed and touches
  nothing outside `<data>/workdirs`.
  A missing last-used timestamp is unknown, never ancient: the TTL counts from
  the sweep that first sees it. Every close goes to the session's hub in
  `closed_sessions`.

### Accounts and usage limits

[0013](docs/decisions/0013-accounts-fail-over-and-limited-runs-wait.md).

- Each account has a harness home: `<data>/accounts/<harness>/<label>/`, passed as
  `CLAUDE_CONFIG_DIR` or `CODEX_HOME`. `yad account add claude work` runs the
  harness's own login with that home.
- Each harness's transcripts live once, in `<data>/transcripts/<harness>/`,
  linked into every account home (`projects/` for Claude, `sessions/` for Codex),
  so any account can resume any session. **Unverified — the first task of the
  accounts epic proves or kills it.**
- **Detection**: Codex publishes `account/rateLimits/updated` with each window's
  use and reset; Claude reports a limit in its result with a reset time.
- **On a limit**: mark the account limited until its reset → the free account
  whose window resets soonest ([0039](docs/decisions/0039-accounts-log-in-themselves-and-the-soonest-reset-goes-first.md)) → resume the same session with a continuation turn. None free →
  the run becomes **waiting** with `resumes_at`, holds no process, and survives a
  restart. While every account of a harness is limited, the runner stops claiming
  for it.

## §4 Local state and configuration

### Profiles and paths

| | default profile | `--profile work` |
|---|---|---|
| config | `~/.config/yad/` | `~/.config/yad/profiles/work/` |
| data | `~/.local/share/yad/` | `~/.local/share/yad/profiles/work/` |

`$YAD_CONFIG_DIR` and `$YAD_DATA_DIR` override both. The config directory holds
`config.toml`, `runner-id` and `credentials/<connection>` (each `0600`); the
data directory holds `state.db`, `workdirs/`, `repos/`, `accounts/`,
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
accounts        = ["personal", "family"] # failover order

[harness.codex]
sandbox  = "danger-full-access"   # the owner's call — 0036; this and never are the defaults when unset
approval = "never"
cap      = 2
accounts = ["personal"]

[[connection]]
name = "yashiki"
url  = "https://ashikaga.tail.ts.net/yad/v1"
cap  = 2

[sessions]
idle_ttl   = "336h"   # close sessions idle this long; "0s" keeps them — 0035
disk_floor = "5GiB"   # below this free under the workdirs, idle sessions go; "0" is off

[drain]
wait = "30m"   # how long a drain lets runs finish before cancelling them — 0029

[workdirs]
roots         = ["/home/me/src"]  # where path sources and local git URLs may point — 0033; none = refused
git_timeout   = "10m"
setup_timeout = "15m"
```

### `state.db`

SQLite (WAL, `0600`) via `modernc.org/sqlite`; queries in `internal/store/*.sql`,
Go generated by `sqlc` and committed. Tables: `sessions`, `runs`, `events`
(the spool — unique `(connection, run_id, seq)`), `outbox`, `accounts` (limit
state), `slots`; a session row keeps the sources its workdir was built from. Session and run ids are the hubs', so every one is keyed with
its connection: two hubs may pick the same id. The schema version is `PRAGMA
user_version`; migrations are embedded and run on open.

### `hub.db`

`yad hub`'s own store, a separate SQLite file opened the same way, with its
queries in `internal/hub/store/*.sql`. Tables: `registration_tokens`,
`runners` and `admin_tokens` (secrets only as SHA-256 hashes), `sessions` (the runner each is bound
to), `runs`, `events` (unique `(run_id, seq)`) and `results` (one per run). A
run's hub-side state adds two before the protocol's: `queued` and `offered`.

## §5 The local surface

```
yad doctor                         what is installed, and what YAD can drive
yad harnesses [--json]             the capability document, as a hub receives it
yad connect <url> --token T|-      register with a hub (- reads the token from stdin — 0020)
yad disconnect <name>
yad daemon start|stop|restart|status|logs [-f] [-n N]
                                   the runner process
yad status [--json]                connections, capacity, runs, sessions and recent
                                   errors — via the socket
yad sessions [--json]              the sessions held: workdir, runs, last use — read
                                   from state.db read-only, so the daemon may be down
yad sessions close [--connection c] <id>
                                   close a session and reclaim its workdir, via the
                                   daemon; one with a run held closes when it ends
yad account add|list|use|remove
yad service install|uninstall|status
                                   launchd user agent, systemd user unit (0028)
yad hub serve                      the standalone hub: protocol at /v1, service API at /api/v1
yad hub submit --harness h --model m [--session id | --new-session id] <instruction | ->
                                   queue a run; prints its id (--watch follows it)
yad hub watch <run>                a run's events as they arrive, then its result;
                                   exits non-zero unless it succeeded
yad hub cancel <run>               stop a run: at once when no runner started it,
                                   else by its runner, down the cancel ladder
yad hub interrupt <run>            end a run's turn, keep its session
yad hub steer <run> <text | ->     add input to a running turn
yad hub drain <runner>             the runner takes no new runs, finishes those it
                                   holds and exits
yad hub close-session <session>    the session takes no new run; its runner deletes
                                   its workdir
yad hub admin-token create|list|revoke
                                   the service API's tokens; create saves to a 0600
                                   file and prints nothing secret (--out - prints once)
yad hub token create [--ttl 1h] [--runner id]
                                   a one-time registration token; --runner re-registers
                                   that runner, the only way to replace its credential
yad conformance <url>              check any hub against v1
```

`yad daemon start` backgrounds itself — it re-executes `yad daemon start
--foreground` in a session of its own and returns once that process answers on
its socket; `--foreground` is what service units run. Logs are JSON through
`log/slog`, rotated by size; a foreground daemon whose stdout is a file or pipe
(a service unit's `service.log`) writes nothing more there once that log is
open. The control socket is `0600` in a data directory
that must itself be private, one JSON request and answer per connection; its
protocol is internal and unversioned. The daemon holds `yad.lock` with
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

## §7 Testing

- **No test spends a token or touches the network.** Harness detection runs
  against an empty `PATH`; adapters replay fixtures.
- **Two fakes.** `internal/adapter/fake` plays a scripted run in memory, for
  runner and hub logic. Child-process behaviour — hangs, ignored `SIGTERM`,
  oversized lines, orphaned grandchildren — is tested by re-executing the test
  binary as the child (`SUPERVISE_TEST_CHILD=<mode>`). The Claude adapter's
  tests re-execute it as a fake `claude` that plays a recorded stream and reads
  stdin as Claude does (`CLAUDE_TEST_FIXTURE=<file>`); the Codex adapter's, as
  a fake `codex app-server` that answers each request the recording answered
  (`internal/adapter/codex/codextest`, `CODEX_TEST_FIXTURE=<file>`).
  Re-executed children set `GORACE=atexit_sleep_ms=0`, or each costs a second.
- **The runner is tested against `yad hub`**, in process, on a random port. The
  conformance suite is the same tests pointed at a URL.
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
- Table-driven, `t.Setenv`, `t.TempDir`, no assertion library; `-race` always.

## §8 Security

- The runner runs as an ordinary user, never root; the service units say so.
- Tokens: `0600` files, never logged, never printed, never in argv, never in an
  event. Grants are deleted when their run ends. An `env` grant is `NAME=value`
  in the harness's environment; a `file` grant is a `0600` file whose path is
  in `NAME`. A grant is named as the secret it is — upper case, ending in
  `_TOKEN`, `_KEY`, `_SECRET`, `_PASSWORD` or `_CREDENTIAL(S)` — and never a
  reserved name or in a loader, runtime or harness namespace (`LD_*`,
  `NODE_*`, `ANTHROPIC_*`, `IS_SANDBOX` …). `protocol/v1` checks it; a run
  carrying one that fails is refused by the hub and by the runner, never run
  with it stripped — [0024](docs/decisions/0024-grants-are-named-as-secrets.md).
  Superseded by [0038](docs/decisions/0038-the-owner-trusts-the-hubs-it-connects.md)
  and changing with DEV-30: any valid name except `PATH`, `HOME`, `LD_*` and
  `DYLD_*`.
  The service API never returns a grant.
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
  outside the owner's `[workdirs] roots` —
  [0033](docs/decisions/0033-sources-reach-only-what-the-owner-allows.md); with
  no roots set they will default to the home directory (0038, DEV-30).
- `claude -p` loads a repository's `.claude/settings.json` hooks and `.mcp.json`
  servers without a trust prompt. With auto-approve that is no worse than the run
  itself — which is exactly why a runner belongs on a machine you would let the
  repository run code on.

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
