# Design

What YAD is, what it talks to, and the order it gets built in.
`DOMAIN.md` owns the words; this file owns the shape.

## §0 The problem, stated once

Two of Slava's systems can decide what an agent should do and neither can make it
happen anywhere but the machine it is installed on.

- **Zumino** holds work and a queue. Its agents are participants with a PAT that
  read `GET /queue`, pull `…/tasks/{n}/context`, write their half of the spec and
  move the status. There is no executor registry by design — nothing in Zumino
  knows a machine exists.
- **Yashiki** already runs `claude -p --resume` with a job queue and a scheduler,
  but on one host, as one process, with no way to say "run this on the Linux box
  instead".

So: a small, boring, outbound-only process that can be installed on any machine,
that advertises what it has, and that runs agents on request. One binary, two
control planes, no inbound ports.

## §1 Shape

```
  ┌──────────────┐        register / heartbeat (fingerprint)
  │   Zumino     │◀───────────────────────────────────────┐
  │  (queue)     │        claim (long poll)               │
  └──────────────┘◀──────────────────────────────┐        │
                           events / result ──────┤        │
  ┌──────────────┐                               │        │
  │  Yashiki     │◀──── same protocol, other ────┤        │
  │ (assistant)  │      driver                   │        │
  └──────────────┘                               │        │
                                        ┌────────┴────────┴────────┐
                                        │        yad runner        │
                                        │  identity · capabilities │
                                        │  claim loop · supervisor │
                                        │  sessions  · control sock│
                                        └────────────┬─────────────┘
                                     ┌───────────────┼───────────────┐
                                  claude           codex          (adapter)
                                   -p --resume     exec --json
```

**Outbound only.** The runner opens connections; nothing connects to it. That is
what lets a laptop on a café network and a VPS behind a firewall be the same kind
of thing, and it removes authentication-of-inbound entirely.

**One control plane per runner.** Two would mean arbitrating two queues for one
pool of concurrency, and the first version of that arbitration is always wrong.
Two control planes means two runners — profiles make that cheap.

## §2 The protocol

Version-prefixed, JSON over HTTPS, bearer token. Nothing in it is agent-specific:
a control plane never learns what a rollout file is.

| | |
|---|---|
| `POST /v1/runners/register` | full capability document → lease, poll config, server time |
| `POST /v1/runners/{id}/heartbeat` | fingerprint + live run ids + load → control messages (cancel, re-report, drain, update) |
| `GET /v1/runs/claim?wait=30s` | long poll; `204` on timeout, one run on success |
| `POST /v1/runs/{id}/events` | batched append of the run's event stream |
| `POST /v1/runs/{id}/result` | terminal state: ok, failed, cancelled, timed out |

**Long poll before websockets.** A 30-second poll is a few dozen requests an
hour, survives every proxy, and needs no reconnect logic. The upgrade path is one
driver method, and taking it before there is a load problem is the wrong order.

**Cancel rides the heartbeat and the poll response** rather than needing a
channel of its own — the runner is always in one of the two, so worst-case
latency is the heartbeat interval.

**The claim is a claim, not a dispatch.** The runner asks for work when it has
capacity; the control plane never pushes. A runner at its concurrency limit
simply stops asking, which is the whole of backpressure.

## §3 Running an agent

A run is: resolve the session → prepare the working directory → spawn → stream →
reap.

- **Spawn non-interactively with structured output** — `claude -p
  --output-format stream-json --resume <id>`, `codex exec --json`. Parsing a TUI
  is not on the table.
- **One process group per run**, so cancel is a signal to the group and a child
  `npm` cannot outlive its parent. SIGINT, then SIGKILL after a grace period.
- **Two watchdogs**: a wall-clock cap per run, and an inactivity timeout on the
  event stream. The second is what actually catches a wedged agent — a hung
  process still holds its pipes open and would otherwise sit there forever.
- **Sessions map, they do not wrap.** YAD stores `session_id → (agent, native
  id, workdir)` and hands the native id back to the CLI. It never reimplements
  conversation state.
- **Working directories are the runner's**, under
  `~/.local/share/yad/sessions/<id>`, unless the run names an existing checkout.
  Reclaiming them is a GC pass, not a delete-on-finish: a session that will be
  resumed wants its checkout warm. Multica's three-mode GC — full, orphan,
  artifacts-only for open work — is the model worth copying.

## §4 Security

- One token per driver, in `~/.config/yad/config.toml`, mode `0600`. Never
  logged, never in `yad agents` output, never in an event.
- **Permission bypass is opt-in per agent, per runner, in config** — never a flag
  the control plane can set. A remote queue must not be able to talk a machine
  into `--dangerously-skip-permissions`.
- The runner runs as an ordinary user, not root. The service unit says so.
- Event streams are agent output and can contain anything the repo contains;
  they are treated as data end to end and never as instructions to YAD itself.

## §5 Milestones

| | | |
|---|---|---|
| **M0** | ✅ scaffolding | detection, capability document, runner identity, heartbeat loop, `doctor`, CI |
| **M1** | daemon for real | control socket, `daemon start/stop/status/logs`, background + log file, `service install` (launchd, systemd user unit) |
| **M2** | one agent, end to end | the claude adapter, sessions store, process supervision, watchdogs, cancel — driven by a fake control plane in tests |
| **M3** | the Zumino driver | register, claim from `GET /queue`, pull task context, stream events, write results back. First real work executed |
| **M4** | codex, then yashiki | the second adapter proves the interface; the yashiki driver proves the driver interface |

Later, deliberately not now: ACP instead of per-CLI adapters, websockets,
workspace GC, a fleet view, more than one control plane per runner.

## §6 Decisions already made

Recorded so they are not relitigated:

- **Go, not Rust.** Not on merit — because `multica/server/pkg/agent` is ~14k
  lines of exactly the hard part (streaming parse per CLI, ACP, process trees,
  cancel semantics) in readable Go under a modified Apache-2.0 licence, and
  because one static cross-compiled binary is what a fleet wants. Rust would fit
  `gpi-tools` house style and start the adapters from zero.
- **Generic core, Zumino first.** The alternative — being a Zumino agent and
  nothing else — is days faster and permanently single-purpose, and yashiki is
  already the second caller.
- **Standard library only, for now.** Reviewed per dependency, not by default.
