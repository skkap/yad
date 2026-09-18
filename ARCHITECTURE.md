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
internal/config          profiles, config.toml, credentials on disk
internal/store           SQLite: schema, migrations, sqlc-generated queries
internal/harness         the catalog, detection, versions
internal/capability      the capability document and its fingerprint
internal/hostool         probing host tools (gh, git, docker, zumino)
internal/account         accounts per harness, homes, limit state, failover
internal/adapter         the Adapter interface and event normalisation
internal/adapter/claude  stream-json both ways
internal/adapter/codex   app-server JSON-RPC
internal/adapter/fake    a scripted harness for tests
internal/supervise       spawn, process groups, watchdogs, cancel ladder
internal/workdir         sources, bare caches, worktrees, setup hook, slots, GC
internal/runner          connections, sync loop, capacity, executor, spool, outbox
internal/hubclient       the runner side of the protocol
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

A request missing `Yad-Protocol` is refused with `422`, like any other invalid
request.

### Sync

```
→ { runner_id, fingerprint, capabilities?,        // document only when asked
    health: { load, free_capacity: {total, by_harness}, disk_free_bytes,
              harnesses: [{id, ready, accounts: [{label, limited_until?}]}],
              spool_depth, outbox_depth, recent_errors[] },
    runs: [{ run_id, state, resumes_at?, reason? }] }   // every run held
← { next_sync_ms, lease_ms,
    runs: [Run],                                  // never more than free capacity
    controls: [{ kind, run_id?, session_id?, text? }],
    min_version? }
```

- **Control kinds**: `cancel`, `interrupt`, `steer`, `close_session`, `drain`,
  `report_capabilities`, `update` (reserved —
  [0018](docs/decisions/0018-no-self-update-in-v1.md)).
- **Claim by listing.** A run offered in a sync response is claimed when the
  runner lists it in its next sync. An offered run that the next sync does not
  list was never received, and the hub offers it again. The runner takes the
  capacity *before* it syncs, so it can always start what it is offered.
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
travel in syncs; the terminal one travels in the result.

### Events

```
{ seq, at, kind, text?, tool?: { id, name, input?, output?, truncated? },
  status?, usage?: { model, input, output, cache_read, cache_write, cost_usd? },
  error?: { class, message } }
```

`kind` is the closed set in `DOMAIN.md`. Batches go out every second or every
100 events, from the SQLite spool, so a network loss drops nothing; the hub's
`acked_through` is authoritative and the runner resends after it. Tool output is
capped at 8 KiB per event.

### Result

```
{ state, final_text?, error?: { class, message },
  usage: { by_model: {...} },
  metrics: { duration_ms, first_event_ms, tool_calls, api_retries, stalls,
             cancel_latency_ms?, waited_ms?, account_switches },
  last_seq }
```

Written to the outbox before the first attempt, deleted on a 2xx, retried with
backoff to five minutes and replayed at every start. A `409` means the hub
already has a different terminal state; the runner keeps the hub's.

### Versioning

The major version is in the path. Within it, both sides advertise feature
strings — the runner in its capability document, the hub in its register
response — and nothing is used that the other side did not advertise. A hub may
refuse a runner below `min_version` with `version_too_old` and a next action.

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
	Wait() Outcome // terminal state, final text, usage, native session id, limit
}
```

**Claude** — [0006](docs/decisions/0006-claude-by-stream-json-codex-by-app-server.md):

```
claude -p --input-format stream-json --output-format stream-json --verbose
       --include-partial-messages --permission-mode <owner config>
       (--session-id <uuid> | --resume <uuid>) [--model m]
       --append-system-prompt-file <context file>
```

The instruction is one stream-json `user` frame on stdin, written from its own
goroutine; stdin stays open for `control_request` (interrupt) until `result`.
YAD chooses the session id, so nothing has to be scraped; an echoed id that
differs means the resume silently failed. `AskUserQuestion` is disallowed —
headless, it returns an empty answer.

**Codex** — `codex app-server --listen stdio://`: `initialize` → `initialized` →
`thread/start` or `thread/resume` → `turn/start`; `turn/interrupt`, `turn/steer`;
approvals answered from the owner's configured policy. The thread id is the
native session id, captured from `thread/started`. Messages are filtered by
thread id — Codex multiplexes subagent threads on one pipe. Validated against
the schema `generate-json-schema` emits for the installed version.

**Fixtures.** Every adapter test replays recorded JSONL named by harness version
(`testdata/claude-2.1.276/*.jsonl`). Recording new ones is a manual step, behind a
build tag; the suite never runs a real harness.

### Supervisor

- **One spawn point.** Every child — harness, git, setup hook, `--version` probe —
  starts through `supervise.Start`: its own process group, a scrubbed environment
  (`CLAUDECODE`, `CLAUDE_CODE_ENTRYPOINT`, `ANTHROPIC_API_KEY` unless configured,
  anything `YAD_*`), stdout lines capped at 32 MiB, stderr tail kept at 2 KiB.
- **Cancel ladder**: the adapter's interrupt → 10 s → `SIGTERM` to the group →
  5 s → `SIGKILL` to the group. Descendants are killed even after the leader
  exits cleanly — they hold pipes and git locks.
- **Watchdogs**: inactivity on the event stream (owner default 30 min; a run may
  lower it) and an optional wall-clock cap. "Force-stopping a healthy run throws
  away the work" — the inactivity default errs long.
- **Exit 0 is not success.** Only the harness's result event decides the state;
  `prompt_too_long` arrives with `subtype: success`.

### Sessions and workdirs

- The session store maps `session → (harness, native id, account, workdir,
  last used, state)`. The native id is written the moment it is known, not at the
  end — a crash must not lose the resume pointer.
- **One live run per session**, enforced by the store.
- **Workdir** per session under `<data>/workdirs/<session>/`. Git sources come from
  a bare cache per repository (`<data>/repos/<hash>.git`, fetched before every
  checkout) as a worktree on the run's branch; a `path` source is used in place
  under a per-path lock. No sources → an empty directory.
- **Setup hook**: if the worktree has an executable `.worktree/setup`, it runs
  with `WT_ROOT`, `WT_MAIN` (the bare cache), `WT_BRANCH`, `WT_SLUG`, `WT_REPO`
  and `WT_SLOT` — the contract in `~/my/gpi-tools/docs/worktrees/README.md`.
  Slots are allocated per repository per runner and recycled when a workdir is
  reclaimed.
- **Reclaiming** — [0011](docs/decisions/0011-hub-closes-sessions-runner-collects.md):
  on `close_session`, on the idle TTL (default 14 days, reported as expired), and
  oldest-idle-first under disk pressure. A first collection after an upgrade
  treats a missing timestamp as unknown, never as ancient.

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
- **On a limit**: mark the account limited until its reset → next free account in
  owner order → resume the same session with a continuation turn. None free →
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
`transcripts/`, `logs/` and the control socket `yad.sock`.

### `config.toml`

```toml
name     = "ashikaga"
labels   = ["macos", "home"]
capacity = 4

[harness.claude]
permission_mode = "bypassPermissions"   # the owner's call — 0015
cap             = 3
accounts        = ["personal", "family"] # failover order

[harness.codex]
sandbox  = "danger-full-access"
approval = "never"
cap      = 2
accounts = ["personal"]

[[connection]]
name = "yashiki"
url  = "https://ashikaga.tail.ts.net/yad/v1"
cap  = 2

[sessions]
idle_ttl = "336h"
```

### `state.db`

SQLite (WAL, `0600`) via `modernc.org/sqlite`; queries in `internal/store/*.sql`,
Go generated by `sqlc` and committed. Tables: `sessions`, `runs`, `events`
(the spool — unique `(run_id, seq)`), `outbox`, `accounts` (limit state),
`slots`, `meta` (schema version). Migrations are embedded and run on open.

## §5 The local surface

```
yad doctor                         what is installed, and what YAD can drive
yad harnesses [--json]             the capability document, as a hub receives it
yad connect <url> --token T        register with a hub
yad disconnect <name>
yad daemon start|stop|status|logs  the runner process
yad status                         runs, sessions, accounts, connections — via the socket
yad sessions [close <id>]
yad account add|list|use|remove
yad service install|uninstall      launchd user agent, systemd user unit
yad hub serve|submit|watch|token   the standalone hub
yad conformance <url>              check any hub against v1
```

`yad daemon start` backgrounds itself; `--foreground` is what service units run.
Logs are JSON through `log/slog`, rotated by size. The control socket is `0600`
in the data directory; its protocol is internal and unversioned.

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
  binary as the child (`SUPERVISE_TEST_CHILD=<mode>`). A fake harness that is a
  real process speaking stream-json arrives with the Claude adapter in E2.
  Re-executed children set `GORACE=atexit_sleep_ms=0`, or each costs a second.
- **The runner is tested against `yad hub`**, in process, on a random port. The
  conformance suite is the same tests pointed at a URL.
- **Real harnesses** only behind `//go:build realharness` and
  `YAD_REAL_HARNESS=1`, run by hand.
- Table-driven, `t.Setenv`, `t.TempDir`, no assertion library; `-race` always.

## §8 Security

- The runner runs as an ordinary user, never root; the service units say so.
- Tokens: `0600` files, never logged, never printed, never in argv, never in an
  event. Grants are deleted when their run ends.
- Permission mode and sandbox are runner configuration per harness; no protocol
  field can set them — [0015](docs/decisions/0015-owner-environment-is-the-trust-boundary.md).
- A hub is untrusted input; harness output is data. Neither is ever executed or
  interpreted as an instruction to YAD.
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
