# Contributing

Thank you for looking. YAD is small on purpose, and most of what keeps it small
is written down. Reading it first saves a round of review.

## Before you start

- **Open an issue first** for anything bigger than a typo or an obvious bug.
  The maintainer's backlog is kept privately, so the issue is where a change is
  agreed before anyone spends time on it.
- **Read [DOMAIN.md](DOMAIN.md)** before naming anything. It owns the
  vocabulary: say *harness* rather than agent, *hub* rather than control plane,
  *run* rather than task or job, *workdir* rather than workspace.
- **Check [docs/decisions/](docs/decisions/)** before proposing something that
  sounds like a better idea. It may already have been weighed, and the record
  says why it went the other way.
- **[ARCHITECTURE.md](ARCHITECTURE.md)** is the shape: packages, the protocol,
  how each harness is driven, local state.

## Building and checking

You need Go 1.27, [sqlc](https://sqlc.dev) 1.31 and
[shellcheck](https://www.shellcheck.net).

```bash
make check      # lint, test with -race, build, generated-file drift, cross-compile
make build      # ./bin/yad
make generate   # regenerate sqlc output and the OpenAPI documents, then commit them
```

`make check` passing is the bar for a pull request ([CHECKS.md](CHECKS.md)).
CI runs a subset of it on a clean Linux machine.

## How the code is written

- **Comments say why, never what.** People read this repository to decide
  whether to trust it on their machine.
- **Errors carry the next action.** A command in an error message is built with
  `shellword.Command` so it runs exactly as printed, and it is tested by
  running it through a real `sh`.
- **Tests never spend a token and never touch the network.** Adapters replay
  recorded fixtures, harnesses are faked by the test binary, and the runner is
  tested against `yad hub` in process. Table-driven tests, `t.Setenv` over
  globals, no assertion library, `-race`.
- **Dependencies are curated.** Each Go module is listed in
  `ARCHITECTURE.md §6` with its reason. Adding one is a reviewed change with a
  line there.
- **The protocol is a public surface.** `protocol/v1` types generate
  `openapi.yaml`, and hubs are generated from that. A rename or removal is a
  new protocol version, never an edit; `make check-breaking` enforces it
  against the last release.

The hard lines — never log or transmit a token, permission mode is never a
protocol field, the runner opens no network port, the CLI never writes
`state.db` — are in [AGENTS.md](AGENTS.md) under *Guardrails*. A change that
crosses one needs a decision record first.

## Pull requests

Branch off `master`, keep one topic per pull request, and fill in the template.
A test that fails without your change is the best evidence that it is needed.

By contributing, you agree that your contribution is licensed under the
[MIT License](LICENSE), like the rest of the project.
