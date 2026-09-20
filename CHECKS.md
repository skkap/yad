# Local checks

What to run before pushing. This file is the authority — `fly` and anything else
that ships work reads it here and runs nothing else.

**The local bar is the whole bar.** CI runs a deliberate subset (`make ci`, and
see the end of this file), because its one advantage over the machine you are
sitting at is that it is a clean Linux box. Everything whose answer is the same
on any machine runs here and not there. **A green CI is therefore not evidence
that a branch is ready; a green `make check` is.**

YAD is a single Go module. Its dependencies are a short curated list
(`ARCHITECTURE.md §6`), fetched by the Go toolchain on first build, so there is
no separate install step. Two things are generated and committed — the sqlc
query code and the protocol's `openapi.yaml` — and the bar includes proving they
are current.

## Module
Setup: none — needs `sqlc` 1.31.1 on PATH (`brew install sqlc`)
Timeout: 6m

```bash
make check
```

which is, in order:

```bash
gofmt -l .                       # must print nothing — make lint turns output into a failure
go vet ./...
go tool staticcheck ./...        # pinned in go.mod as a tool, so everyone runs the same version
CGO_ENABLED=1 go test -race ./...
go build ./cmd/yad
make generate && git diff --exit-code internal/store/db internal/hub/store/db protocol/v1/openapi.yaml protocol/hubapi/openapi.yaml
GOOS=… GOARCH=… go build ./...   # linux/amd64, linux/arm64, darwin/arm64, darwin/amd64
GOOS=… GOARCH=… go vet ./...     # the same four — type-checks the tests for each target
```

**Generated files.** A change to `internal/store/*.sql`, `internal/hub/store/*.sql`, or to a `protocol/v1`
or `protocol/hubapi` type needs `make generate` and the regenerated files in the same commit. The
check also fails on a generated file that exists but was never added — the
usual way a new sqlc output file goes missing from a PR.

**The protocol is a public surface.** A diff in `protocol/v1/openapi.yaml` is
a change every TypeScript hub will feel. If it renames or removes anything, it
belongs in v2, not in this PR.

**Cross-compilation** is part of the bar because the runner is developed on
macOS and deployed on Linux: anything reaching for `syscall` outside a `unix`
build tag, or for cgo, is caught here and nowhere else. The build never
compiles test files, so each target is vetted too: a test using a symbol one
OS lacks — `syscall.Getsid` exists on darwin only — fails there rather than in
CI. Everything builds with
`CGO_ENABLED=0`; only the race detector turns cgo back on, for tests.

## What CI runs, and what it does not

CI is one job: `make ci` on `ubuntu-latest`, which is

```bash
go vet ./...            # type-checks the tests too, which a build never does
go build ./cmd/yad
go test ./...           # no -race
```

That is the part whose answer can differ on Linux, plus `vet`, which is kept
despite being machine-independent because it type-checks test files — twice a
merge here has been textually clean and failed to compile, and only `vet` saw
it.

**CI does not run** `gofmt`, `staticcheck`, `check-generated`, the
cross-compile matrix, or the race detector. Not because they do not matter —
they are in `make check` and `make check` is the bar — but because they answer
the same on any machine, and paying a hosted runner to repeat a local answer
buys nothing. `-race` is the exception that is about cost rather than
duplication: it needs cgo and roughly triples the suite.

So, concretely, **these reach master only if someone ran `make check`**: a
formatting slip, a staticcheck finding, a stale or uncommitted generated file,
a darwin-only symbol that breaks the linux build, and a data race. `fly` runs
`make check` before it pushes, which is what makes that safe.

## What nothing runs, here or in CI

- **No harness is invoked.** Detection is tested against an empty `PATH`,
  children are the test binary re-executed as a fake, and adapters replay
  recorded fixtures. Nothing in the suite spends a token or needs `claude` or
  `codex` on the box. Tests against real harnesses sit behind the `realharness`
  build tag and `YAD_REAL_HARNESS=1`, and are run by hand when recording new
  fixtures. Every end-to-end test runs once per harness, each against its
  fake. `make smoke` runs one real Claude run through `yad hub` end to end,
  and `make smoke-codex` one real Codex run (`scripts/smoke.sh <harness>`, the
  cheapest model unless `SMOKE_MODEL` says otherwise); they spend tokens and
  need the harness installed and logged in, so they are run by hand, never
  here or in CI.
- **No hub is contacted.** The runner is tested against `yad hub` in process,
  and `yad hub` against the conformance suite over HTTP on a local listener.
  There is no test against Zumino or yashiki; `yad conformance <url>` is how
  one of those is checked, by hand.
- **No release is fetched.** `yad upgrade` and `scripts/install.sh` both go
  through `gh`, so both are tested against a `gh` that is a shell stub on
  `PATH` and a release made of files on disk. Nothing here downloads a real
  release, and no check on this machine proves the install script on a fresh
  Linux VM.
- **No breaking-change check against a released spec yet.** There is no
  released v1 to compare with; `oasdiff` joins the bar with the first release
  (epic E7).
