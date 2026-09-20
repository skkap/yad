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
drives Claude Code is built and tested against recorded streams, and the
executor hands it runs: a runner advertises the capacity it has free, claims
what its hubs offer within it, and reports events and results back. The build
order is `ARCHITECTURE.md §9`; the epics and tasks are in Zumino, project
`yad/dev`.

## Install

The repository is private, so there is no URL to download from without a token
— for the binaries or for the install script. `gh` does the fetching, and the
GitHub login you already have is what grants access:

```bash
gh api -H "Accept: application/vnd.github.raw" \
  repos/skkap/yad/contents/scripts/install.sh > yad-install.sh &&
  sh yad-install.sh
```

The `&&` is the point, not the two steps. A pipeline reports only its last
command's status, so `gh api … | sh` hands `sh` an empty stream and exits 0
having installed nothing — and so does `gh api … > f` followed by a separate
`sh f`, because the redirection creates the file whether `gh` succeeds or not
and `sh` on an empty file exits 0. Joined with `&&`, a `gh` that cannot fetch
the script fails the whole command.

The file is kept because the pinning example below re-runs it — not so you can
read it first: the `&&` runs it as soon as the fetch succeeds. To read it
before it runs, fetch and run as two separate commands, and check `gh`'s exit
status yourself, because the redirection creates the file whether `gh`
succeeded or not.

It puts `yad` in `~/.local/bin`, checks the release's SHA-256 before writing
anything, and tells you if that directory is not on your `PATH`. Then
`yad doctor`.

Fetch it with `gh` rather than `curl`: a `curl` carrying
`Authorization: Bearer $(gh auth token)` puts the live token in `curl`'s argv,
where `/proc` and `ps` hand it to every local account for the length of the
request.

`YAD_VERSION` pins a release and `YAD_INSTALL_DIR` moves where it lands:

```bash
YAD_VERSION=v0.3.1 sh yad-install.sh   # the file the command above left behind
```

Later, on your command and never on its own:

```bash
yad upgrade --check       # what the newest release is; changes nothing
yad upgrade               # fetch it, verify its checksum, then replace this binary
yad upgrade --tag v0.3.1  # that release, newer or older — how a bad one is rolled back
```

If you installed from a fork, set `YAD_REPO` for the upgrade too — nothing
records where the binary came from, so an upgrade without it would replace your
fork's build with upstream's.

`yad upgrade` downloads to a temporary directory beside the installed binary,
checks its SHA-256 against the release's `checksums.txt`, and only then renames
it into place — so an upgrade that fails at any step leaves a working `yad`. It
restarts nothing: a runner already running holds the binary it started from
until you restart it, and `yad upgrade` says so — naming the profile, and
carrying it in the commands it offers. If that runner is a service, re-run
`yad service install [--profile name]` rather than `yad daemon restart`:
install replaces the unit and starts it again, where a restart would leave an
unsupervised process the service manager is no longer watching
([0028](docs/decisions/0028-a-runner-is-a-per-user-service-with-its-login-path.md)).
The profile goes after `service install` but *before* `daemon` —
`yad --profile work daemon restart` — because `daemon` has no flag of its own. Nothing in
YAD updates itself on a schedule or on a hub's say-so
([0018](docs/decisions/0018-no-self-update-in-v1.md)).

Releases are built by CI on a `v*` tag: linux and darwin × amd64 and arm64,
`CGO_ENABLED=0`, with a `checksums.txt` covering all four. To check a download
by hand, pull out the one line for the binary you took — a checker given the
whole file reports the three you did not download as failures:

```bash
grep " yad-linux-amd64$" checksums.txt | sha256sum -c -   # or: shasum -a 256 -c -
```

## Run it safely

A runner auto-approves everything its harness does — nobody is there to answer a
prompt. Run it on a machine, VM or container you would let an unknown repository
execute code on, never on a laptop holding credentials you care about, and one
runner per trust domain: personal and work are two runners.

You trust the hubs you connect, and YAD does not police what they send
([0038](docs/decisions/0038-the-owner-trusts-the-hubs-it-connects.md)). A hub
writes the brief, and a brief can tell the harness to read any file or send any
secret anywhere — so a hub that can queue a run here reaches whatever this
machine reaches, and filtering the names it uses would never have changed that.
Connect the hubs you would hand that much to, and nothing else.

What that means in practice. A run's grants may be named anything a shell
accepts as a variable, except `PATH`, `HOME` and the loader variables `LD_*` and
`DYLD_*` — refused whatever their case, and only because a hub's mistake there
would make every run fail for no visible reason. `IS_SANDBOX` is accepted as a
name and then dropped before Claude starts: only you declare that, in the
runner's own environment. A grant reaches the harness process alone, never the
prompt, the logs or the events, as `NAME=value` or a `0600` file under the
runner's data directory rather than in the checkout, and it is deleted when the
run ends — or at the next start, if the runner was killed before it could. A
folder source is taken only inside `[workdirs] roots` in `config.toml`; with
none listed that is your home directory, so list the directories runs may use if
you want them to reach less.

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
make install    # ~/.local/bin/yad, from this checkout rather than a release
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
