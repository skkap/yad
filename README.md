# YAD

**One small binary that runs coding-agent harnesses on machines you own, for
whatever asks.**

YAD sits on a Mac or a Linux box, notices which harnesses are installed —
Claude Code, Codex — and connects outbound to one or more **hubs**. When a hub
has work, YAD claims a run, prepares a workdir, drives the harness against a
durable session, streams what happened, and reports how it ended. It survives
restarts and usage limits, fails over between accounts, and never opens a port.

A hub is anything that hosts the server half of the runner protocol:

- **[Zumino](https://zumino.cc)** — the task tracker. Its customers can attach
  their own runners and have Zumino's work run on their own machines and
  subscriptions.
- **yashiki** — the resident assistant, which today runs everything on one Mac
  mini.
- **`yad hub`** — the same binary in server mode, for anything that would rather
  not embed the protocol.

YAD is deliberately not a tracker, a UI, an orchestrator or a sandbox. The
vocabulary is in [`DOMAIN.md`](DOMAIN.md), the shape and the protocol in
[`ARCHITECTURE.md`](ARCHITECTURE.md), and the reasons in
[`docs/decisions/`](docs/decisions/).

```
$ yad doctor
HARNESS             STATUS      VERSION                PATH
Claude Code         ready       2.1.278 (Claude Code)  /Users/me/.local/bin/claude
Codex               ready       codex-cli 0.147.0      /Users/me/.local/bin/codex
Gemini CLI          no adapter  0.29.2                 /…/bin/gemini
Cursor Agent        no adapter  2025.09.12-4852336     /Users/me/.local/bin/cursor-agent

profile default — config /Users/me/.config/yad
2 harness(es) this runner can be given work for.
```

Claude Code and Codex are `ready`: each has an adapter. The others are
recognised — reported so the gap is visible, and refused as the target of a
run. A Codex whose app-server protocol differs from the one this yad was built
against is still `ready`, with a `warning:` line under the table saying so.

## Status

**Foundation, and the first half of epic E2.** Harness detection, the capability
document, the v1 protocol types and their generated OpenAPI document, the local
store, the process supervisor, config and profiles. `yad hub` keeps its own
store and serves register and sync: `yad hub token create` issues a one-time
registration token, `yad connect <url> --token -` registers a runner, and `yad
daemon start --foreground` syncs with every connected hub. The adapter that
drives Claude Code is built and tested against recorded streams. Nothing hands
it a run yet: the executor that does is the rest of E2, so a runner advertises
no free capacity and claims nothing, and the events, result and deregister
calls still answer `not_implemented`. The build order is `ARCHITECTURE.md §9`;
the epics and tasks are in Zumino, project `yad/dev`.

## Run it safely

A runner auto-approves everything its harness does — nobody is there to answer a
prompt. Run it on a machine, VM or container you would let an unknown repository
execute code on, never on a laptop holding credentials you care about, and one
runner per trust domain: personal and work are two runners.

Claude Code runs with `--permission-mode bypassPermissions` unless
`permission_mode` under `[harness.claude]` in `config.toml` says otherwise.
Claude refuses that mode as root; run the runner as an ordinary user.

Codex runs with approval policy `never` and sandbox `danger-full-access` unless
`approval` and `sandbox` under `[harness.codex]` say otherwise
([0036](docs/decisions/0036-codex-runs-unsandboxed-and-never-asks-unless-the-owner-says.md)).
`sandbox = "workspace-write"` keeps what Codex writes inside the run's workdir,
and keeps it off the network, so a run cannot push or install. A policy that
asks for approval is answered no: nobody is there to say yes.

## Run it as a service

```bash
yad service install [--profile name]     # launchd agent on macOS, systemd user unit on Linux
yad service status  [--profile name]
yad service uninstall [--profile name]   # stops it and removes the unit; safe to repeat
```

The service runs `yad daemon start --foreground` as you, never as root, and
restarts it after a crash. It uses the PATH your login shell had when you ran
`install`, so run `install` again after changing PATH, upgrading or moving the
binary; a re-install replaces the unit. Stopping the service drains the
runner — no new runs, the ones it holds finish for up to `[drain] wait`
(default 30m), then are cancelled — and the unit's stop timeout is derived
from that wait when you install, so run `install` again after changing it. On Linux a user unit stops when you log
out unless lingering is on — `install` tells you, and `loginctl enable-linger`
is yours to run. Why it is shaped this way: [0028](docs/decisions/0028-a-runner-is-a-per-user-service-with-its-login-path.md).

## Build

```bash
make check      # lint, test, build, generated-file drift, cross-compile — see CHECKS.md
make build      # ./bin/yad
make install    # ~/.local/bin/yad
make generate   # sqlc and the OpenAPI document
```

Go 1.27 and `sqlc` 1.31. Dependencies are a curated list with a reason for each,
in `ARCHITECTURE.md §6`.

## Prior art

Read before adding anything; most of this problem is solved somewhere.

| | What it is |
|---|---|
| **Anthropic self-hosted runners** | The official version of this idea for Claude, and the source of *runner*, *lease* and *release*: outbound polling, the poll is the heartbeat, drain and retire-at |
| **[Multica](https://github.com/multica-ai/multica)** | The closest complete implementation: a daemon that detects CLIs, claims tasks and drives Claude and Codex. Read for shapes only — its licence restricts derived code ([0014](docs/decisions/0014-multica-shapes-never-code.md)) |
| **GitHub Actions, Buildkite and GitLab runners** | Registration-token exchange, leases, idempotent results, chunked logs, cancel on the channel already polled |
| **Paseo, vibe-kanban, happy** | The adapter split this repo follows: Claude by stream-json, Codex by app-server, ACP for the long tail |
| **ACP** (Agent Client Protocol) | The likely answer for the recognised harnesses; not for Claude or Codex, which speak it only through Node adapters |
| **Coder agentapi** | TUI scraping behind HTTP; archived in September 2026. The approach this repo does not take |

## The name

YAD. That is all it is.
