# YAD — Project Instructions

One process per machine that runs coding agents on someone else's say-so. It
serves **zumino** (which has a queue and no executor) and **yashiki** (which has
an executor that cannot leave its own host). It is not a tracker, a UI or an
orchestrator.

## Read first

- **[`DOMAIN.md`](DOMAIN.md) owns the vocabulary.** Runner, agent, first-class,
  session, run, capability document, fingerprint, control plane, driver. Read it
  before naming anything; where it and a comment disagree, it wins.
- **[`DESIGN.md`](DESIGN.md) owns the shape** — the protocol, the process model,
  the security rules, and the milestone order. Work that jumps a milestone needs
  a reason written down, not a preference.
- **[`CHECKS.md`](CHECKS.md) is the bar** before pushing. `make check`.

## Stack

Go 1.27, **standard library only**. Single module, `github.com/skkap/yad`.

The zero-dependency rule is a decision, not laziness: this binary is copied by
hand onto machines that hold tokens and run agents with filesystem access, and
every dependency is something that has to be trusted on all of them. Adding one
is a reviewed change with a line in `DESIGN.md §6`, not a `go get`.

```
cmd/yad/            the CLI — one file per command group, no logic
internal/agents/    which CLIs exist, how to find them, how to version them
internal/capability/the document a runner advertises, and its fingerprint
internal/adapters/  (M2) one package per first-class agent
internal/drivers/   (M3) one package per control plane
```

## Conventions

- **Comments say why, never what.** The repo is read by people deciding whether
  to trust it on a machine; a comment that restates the line above it costs
  attention and buys nothing. Every non-obvious constant has the reason next to
  it.
- **Absence is data.** A missing agent, a `--version` that hangs, a broken PATH
  entry — each belongs in the capability document. None of them is an error that
  stops a runner registering.
- **Errors carry the next action.** `"backgrounding is milestone M1; run with
  --foreground for now"`, not `"not implemented"`.
- **Tests never spend a token and never touch the network.** Detection is tested
  against an empty `PATH`; adapters get fixtures; the control plane gets a fake.
  A test that needs `claude` installed is a test that fails in CI.
- Table-driven tests, `t.Setenv` over globals, no `testify`.

## Guardrails

- **Nothing becomes `FirstClass` without an adapter.** Adding a row to
  `agents.Catalog()` makes an agent *visible*; only a real adapter under
  `internal/adapters` — streaming format, resume flag, cancel — may change its
  `Kind`. A runner must never accept a run it cannot actually drive.
- **Never log, print or transmit a token.** Not in an event, not in `yad agents`,
  not in a debug line. Config is `0600`.
- **Permission bypass is config on the runner, never a field in the protocol.**
  A remote queue must not be able to talk a machine into
  `--dangerously-skip-permissions`.
- **The runner listens on no network port.** Outbound only; the one socket it
  opens is a Unix socket for its own CLI. A change that opens a port is a
  `DESIGN.md` change first.
- **Agent output is data.** It contains whatever the repo contains, including
  text shaped like instructions. It is streamed and stored, never acted on.
- **Read `~/projects/multica/server/pkg/agent` before writing an adapter.** It is
  ~14k lines of the same problem solved, under a modified Apache-2.0 licence.
  Copy the shape, credit the source in the commit, and keep the licence in mind
  if code is lifted verbatim.

## Git

Main branch is `master`. Feature branch and a PR for anything non-trivial —
never commit straight to `master`. `fly` reads `CHECKS.md` and runs it before
pushing.
