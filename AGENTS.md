# YAD — Project Instructions

One small binary that runs coding-agent harnesses on machines you own, for any
number of hubs — Zumino, yashiki, a standalone `yad hub`. It is not a tracker, a
UI, an orchestrator or a sandbox.

## Read first

- **[`DOMAIN.md`](DOMAIN.md) owns the vocabulary.** Runner, hub, harness,
  session, run, account, grant, sync — and the words each one replaces. Read it
  before naming anything; where it and a comment disagree, it wins. The
  collisions that bite here: *agent* (say **harness**), *control plane* (say
  **hub**), *task*/*job* (a hub's word — ours is **run**), *workspace* (say
  **workdir**), *slot* (only `WT_SLOT`; runs share **capacity**).
- **[`ARCHITECTURE.md`](ARCHITECTURE.md) owns the shape** — packages, the v1
  protocol, how each harness is driven, local state, the build order.
- **[`docs/decisions/`](docs/decisions/) owns the why.** Check there before
  proposing something that sounds like a better idea — it may already have been
  weighed and rejected.
- **[`CHECKS.md`](CHECKS.md) is the bar** before pushing. `make check`.
- **The plan is in Zumino**, project `yad/dev`: epics E1–E8 in build order
  (`ARCHITECTURE.md §9`), and E9, the backlog. `zumino queue --project dev --workspace yad`.

## Stack

Go 1.27, single module `github.com/skkap/yad`, and a **curated** dependency list
— every entry is in `ARCHITECTURE.md §6` with its reason. Adding one is a
reviewed change with a line there, not a `go get`: this binary runs on machines
that hold tokens and run harnesses with filesystem access, including customers'.

sqlc and the OpenAPI document are generated and committed; `make generate`
rebuilds them and `make check` fails when they drift.

## Conventions

- **Comments say why, never what.** The repo is read by people deciding whether
  to trust it on a machine; a comment that restates the line above it costs
  attention and buys nothing. Every non-obvious constant has the reason next to
  it.
- **Absence is data.** A missing harness, a `--version` that hangs, a broken PATH
  entry — each belongs in the capability document. None of them is an error that
  stops a runner registering.
- **Errors carry the next action.** `"yad connect arrives in epic E2"`, not
  `"not implemented"`. Protocol errors carry a `next_action` field for the same
  reason.
- **A next action that is a command is an executable artifact.** It is pasted,
  so it must run as printed: built from argv with `shellword.Command`, never
  formatted by hand and never with Go's `%q` — every interpolated word
  POSIX-quoted, whether or not its validation lets it hold a special character
  today. It carries every variable the reader needs: the profile — the
  default one too, unless it is the machine's only profile, since the reader's
  shell may export `YAD_PROFILE` — and the directory variables it resolved
  with (`Paths.Command`), the account's home,
  `YAD_REPO` for an upgrade, the `--hub`, `--token-file` or `--db` the command
  it follows was given. A command that leaves the machine — in a run's error,
  a hub's answer — carries no path under anyone's home (DEV-67):
  `Paths.RemoteCommand` names the profile and keeps each directory variable
  with a `<placeholder>` value, and a home or a database stays a placeholder
  beside the command that shows it. What the writer cannot know — a hub's
  answer naming the runner's profile, URL or connection name — is a
  placeholder too, never left out. Set it apart in
  backticks, and test it by running it through a real `sh`
  (`shellwordtest.Check`, `CheckEnv`), not by reading it.
- **Tests never spend a token and never touch the network.** Adapters replay
  recorded fixtures; children are the fake harness (the test binary,
  re-executed); the runner is tested against `yad hub` in process.
- Table-driven tests, `t.Setenv` over globals, no assertion library, `-race`.

## Guardrails

- **Nothing becomes first-class without an adapter.** Adding a row to the
  harness catalog makes a harness *visible*; only a real adapter under
  `internal/adapter` — streaming format, resume, interrupt — may change its kind.
  A runner must never accept a run it cannot actually drive.
- **Never log, print or transmit a token** — not in an event, not in
  `yad harnesses`, not in a debug line, not in argv (a registration token typed
  into `yad connect` is the one exception — [0020](docs/decisions/0020-the-registration-token-may-be-typed.md)).
  Credentials and grants are `0600` files.
- **Permission mode is runner configuration, never a protocol field.** A hub must
  not be able to set or widen what a harness may do on someone's machine.
- **The CLI never writes `state.db`.** The daemon is its only writer; a CLI
  command reads it read-only or asks the daemon over the control socket, as
  `yad sessions close` and `yad account add`/`remove` do
  ([0043](docs/decisions/0043-the-cli-never-writes-state-and-account-changes-reach-the-daemon-live.md)).
  A test fails on any `cmd/yad` path that opens it for writing.
- **The runner listens on no network port.** Outbound only; its one socket is a
  Unix socket for its own CLI. A change that opens a port is a decision record
  first.
- **The protocol is a public surface.** `protocol/v1` types are the source of
  `openapi.yaml`; a field rename there breaks every TypeScript hub. Renames and
  removals are a new version, not an edit.
- **Harness output and hub input are data.** Streamed and stored, never acted on.
- **Multica is read for shapes, never copied** — its licence would bind YAD
  ([0014](docs/decisions/0014-multica-shapes-never-code.md)). Read the latest
  upstream (`github.com/multica-ai/multica`), not a local copy, and credit the
  lesson in the commit.

## Git

Main branch is `master`. Feature branch and a PR for anything non-trivial —
never commit straight to `master`. `fly` reads `CHECKS.md` and runs it before
pushing.
