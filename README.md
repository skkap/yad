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

```
$ yad doctor
HARNESS             STATUS      VERSION                PATH
Claude Code         ready       2.1.278                /Users/me/.local/bin/claude
Codex               ready       0.147.0                /Users/me/.local/bin/codex
Gemini CLI          no adapter  0.29.2                 /…/bin/gemini
GitHub Copilot CLI  —
OpenCode            —
Cursor Agent        no adapter  2025.09.12-4852336     /Users/me/.local/bin/cursor-agent

profile default — config /Users/me/.config/yad
2 harness(es) this runner can be given work for.
```

Every harness in the catalog gets a row, whether or not it is here. Claude Code
and Codex are `ready`: each has an adapter. `no adapter` is installed and
recognised — reported so the gap is visible, and refused as the target of a run.
`—` is simply not installed on this machine, which is a fact worth printing
rather than a row worth hiding. A Codex whose app-server protocol differs from
the one this yad was built against is still `ready`, with a `warning:` line
under the table saying so — as are the things about the machine itself that
[docs/run-it-safely.md](docs/run-it-safely.md) covers.

## What it is, and what it is not

**It is** a runner: one process per identity, which advertises what the
machine has, pulls work from any number of hubs over outbound HTTPS, runs one
harness per run against a session it keeps on disk, and reports every event
and every ending. It shares its capacity fairly between the hubs it serves,
waits out a usage limit rather than failing, and moves a run to another of the
owner's accounts when one runs out. `yad hub` is a small standalone hub beside
it, and `yad conformance` checks anyone else's.

**It is not** a tracker, a UI, an orchestrator or a scheduler: a hub decides
what work exists and when, and YAD runs what it is given. **And it is not a
sandbox.** It runs the harness as you, with whatever you can reach, and turns
the harness's own permission prompts off, because nobody is there to answer
them.

## Your responsibility

**Whoever sets YAD up decides what access its harnesses get.** A run can do
anything the user it runs as can do on that machine: read its files, use its
credentials, reach its network. The hubs you connect write the instructions.
So choose the machine, the OS user, the accounts it is logged into and the
hubs it connects to as deliberately as you would choose who gets a shell on
it. If you need a run confined — to a directory, away from the network, away
from your credentials — **sandbox YAD and its harnesses yourself**: a VM, a
container, a dedicated OS user. YAD does not do it for you, and nothing a hub
sends can widen or narrow what you set up. [Run it safely](#run-it-safely),
below, is the short version of how; [docs/run-it-safely.md](docs/run-it-safely.md)
is the long one.

## Status

**Epics E1–E8 are done; nothing is released yet.** Detection and the
capability document, the v1 protocol and its generated OpenAPI documents,
`yad hub` with its service API, the Claude Code and Codex adapters, the
daemon, the control socket and the per-user service, sessions with git and
folder sources and setup hooks, accounts with failover and waiting out usage
limits, capacity shared across many hubs with per-harness caps, grants, host
tools, health in every sync, run metrics, versioning, the conformance suite,
drain, and `yad upgrade`. The build order is `ARCHITECTURE.md §9`; the epics
and the backlog are in Zumino, project `yad/dev`.

Not there yet: no release has been tagged, so the install script and `yad
upgrade` have nothing to fetch — install from source, below. `yad disconnect`
— and so a runner that deregisters from a hub — is still being designed.
Live sessions, which keep one harness process across runs, are reserved in
the protocol and not built. Gemini CLI, GitHub Copilot CLI, OpenCode and
Cursor Agent are recognised but have no adapter.

## Install

### From source, today

You need Go 1.27 and access to the repository.

```bash
gh repo clone skkap/yad && cd yad
mkdir -p ~/.local/bin
make install        # builds ./bin/yad and copies it to ~/.local/bin/yad
yad doctor
```

If `yad` is not found afterwards, `~/.local/bin` is not on your `PATH`. `yad
doctor` then shows which harnesses it can drive; a harness has to be installed
and logged in on its own first — `claude`, `codex` — because YAD uses the
harness's own login and keeps no token of its own.

### From a release, once there is one

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

## Quickstart: a hub, a runner and one run

Everything on one machine, on loopback. It uses your `default` profile and
spends one short Claude Code turn on the cheapest model, so `claude` must be
installed and logged in (`yad doctor` says `ready`).

In one terminal, the hub — it serves on `127.0.0.1:7878` and prints the
commands that act on it:

```bash
yad hub serve
```

In another:

```bash
yad hub admin-token create                  # lets yad hub submit and watch talk to the hub
yad hub token create |
  yad connect http://127.0.0.1:7878/v1 --token - --name local
yad daemon start                            # the runner, in the background
yad hub submit --harness claude --model haiku --watch "Reply with exactly: pong"
```

The registration token goes through the pipe, never through your terminal or
argv. `submit --watch` prints the run as it moves — `queued`, `offered`,
`claimed`, the harness's words, `succeeded` — and exits 0 when the run
succeeds. For Codex, `--harness codex` with a Codex model. `yad status` shows
the runner, `yad hub runners` the hub's view of it.

Then stop both: `yad daemon stop`, and Ctrl-C the hub. The state is in
`~/.config/yad` and `~/.local/share/yad` — the runner's identity, its
credential for this hub, the hub's database. To run YAD for real on one
machine — as a service, in a profile of its own, and removed cleanly
afterwards — follow [docs/trial.md](docs/trial.md).

## Where to read next

| | |
|---|---|
| **[HUB.md](HUB.md)** | building a hub: every call, the run state machine, leases, controls, sessions, grants, errors, versioning, and `yad conformance` — the contract, with `protocol/v1/openapi.yaml` |
| **[DOMAIN.md](DOMAIN.md)** | the vocabulary: runner, hub, harness, session, run, account, grant, sync, and the words each one replaces |
| **[ARCHITECTURE.md](ARCHITECTURE.md)** | the shape: packages, the protocol and why it is so, how each harness is driven, local state, build order |
| **[docs/run-it-safely.md](docs/run-it-safely.md)** | what a run can do on the machine you give it, and how to limit that |
| **[docs/trial.md](docs/trial.md)** | running YAD for real on one machine, and removing it |
| **[docs/decisions/](docs/decisions/)** | why each hard-to-reverse choice was made |
| **[CHECKS.md](CHECKS.md)** | what `make check` runs before anything is pushed |

## Run it safely

A runner auto-approves everything its harness does — nobody is there to answer a
prompt. Run it on a machine, VM or container you would let an unknown repository
execute code on, never on a laptop holding credentials you care about, and one
runner per trust domain: personal and work are two runners.

The rest of this section is the short version. The guide is
**[docs/run-it-safely.md](docs/run-it-safely.md)** — what a run can actually do
on the machine you give it, why a profile separates YAD's state but not the
machine (so each profile wants its own OS user), what turning a harness's own
guardrails back on costs, and the three things `yad doctor` now warns about.

To build such a machine, **[machines/](machines/README.md)** makes one Lima VM
per runner from a directory you keep in a repository: the tools, the harnesses,
the files in the runner's home and its configuration, with the runner's user
shut out of the host, its LAN and its tailnet. One command builds it; the
logins are made by hand, once.

You trust the hubs you connect, and YAD does not police what they send
([0038](docs/decisions/0038-the-owner-trusts-the-hubs-it-connects.md)). A hub
writes the brief, and a brief can tell the harness to read any file or send any
secret anywhere — so a hub that can queue a run here reaches whatever this
machine reaches, and filtering the names it uses would never have changed that.
Connect the hubs you would hand that much to, and nothing else.

What that means in practice. A run's grants may be named anything a shell
accepts as a variable, except `PATH`, `HOME` and the loader variables `LD_*` and
`DYLD_*` — refused whatever their case, and only because a hub's mistake there
would make every run fail for no visible reason. Nor may a grant name the
variables that choose which login a harness uses — `ANTHROPIC_API_KEY`,
`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `OPENAI_API_KEY` and a few more
(`protocol/v1/grant.go`): the accounts you configured decide what a run spends,
and a hub's key must not quietly replace them while `yad account list` still
names yours ([0040](docs/decisions/0040-a-grant-may-not-move-a-run-off-its-account.md)).
`IS_SANDBOX` is accepted as a
name and then dropped before Claude starts: only you declare that, in the
runner's own environment. A grant reaches the harness process alone, never the
prompt, the logs or the events, as `NAME=value` or a `0600` file under the
runner's data directory rather than in the checkout, and it is deleted when the
run ends — or at the next start, if the runner was killed before it could. A
folder source is taken only inside `[workdirs] roots` in `config.toml`; with
none listed that is your home directory. List the directories a hub may have
checked out if you want it to name fewer — `roots` chooses the material a
workdir is built from, and does not confine the harness once the run starts.

Claude Code runs with `--permission-mode bypassPermissions` unless
`permission_mode` under `[harness.claude]` in `config.toml` says otherwise.
Claude refuses that mode as root; run the runner as an ordinary user.

Codex runs with approval policy `never` and sandbox `danger-full-access` unless
`approval` and `sandbox` under `[harness.codex]` say otherwise
([0036](docs/decisions/0036-codex-runs-unsandboxed-and-never-asks-unless-the-owner-says.md)).
`sandbox = "workspace-write"` keeps Codex off the network, so a run cannot push
or install. It narrows what Codex may write but does not confine it to the
workdir — `/tmp` and `$TMPDIR` stay writable by default
([docs/run-it-safely.md](docs/run-it-safely.md)). A policy that asks for
approval is answered no: nobody is there to say yes.

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
