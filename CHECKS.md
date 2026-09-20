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
scripts/breaking.sh              # oasdiff, both OpenAPI documents, against the last release tag
GOOS=… GOARCH=… go build ./...   # linux/amd64, linux/arm64, darwin/arm64, darwin/amd64
GOOS=… GOARCH=… go vet ./...     # the same four — type-checks the tests for each target
```

**Generated files.** A change to `internal/store/*.sql`, `internal/hub/store/*.sql`, or to a `protocol/v1`
or `protocol/hubapi` type needs `make generate` and the regenerated files in the same commit. The
check also fails on a generated file that exists but was never added — the
usual way a new sqlc output file goes missing from a PR.

**The protocol is a public surface.** A diff in `protocol/v1/openapi.yaml` is
a change every TypeScript hub will feel. If it renames or removes anything, it
belongs in v2, not in this PR. `make check-breaking` is what stops that by
machine rather than by eye: each document against itself at the last release
tag that is not this commit's own, failing on anything oasdiff rates breaking.
The exclusion is what keeps it honest during a release — `release.yml` runs
`make check` with HEAD detached at the tag being published, and without it the
baseline would be the working tree and the check would compare a file to
itself. It covers
`protocol/hubapi/openapi.yaml` too — a service generates its client from that
file alone ([0022](docs/decisions/0022-hub-service-api-beside-the-protocol.md)),
and the same renamed field showed up as one error in the protocol document and
six in the hub's. Until the owner pushes the first `v[0-9]*` tag there is no
baseline and the check says so on every run rather than passing quietly.

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
make check-breaking     # oasdiff — the one machine-independent check CI repeats
```

That is the part whose answer can differ on Linux, plus two deliberate
exceptions. `vet` is kept despite being machine-independent because it
type-checks test files — twice a merge here has been textually clean and failed
to compile, and only `vet` saw it.

`make check-breaking` is the second, and it breaks the principle rather than
bending the list. oasdiff answers the same anywhere, so the rule says it should
live in `make check` alone. It is in both because of **who it protects**. Every
other check here protects the person who ran it: skip `gofmt` and your own
branch is ugly, skip `cross` and your own build breaks on Linux. This one
protects hubs already generated from a released document, running on machines
nobody here can reach, whose authors find out at runtime. `make check` is the
bar and this file says so — and this file cannot bind a contributor who never
opened it. Seconds of hosted runner against somebody else's outage is not a
close trade. It needs tags to have a baseline, so `ci.yml` checks out with
`fetch-depth: 0`; without that it would find no tag and pass for ever.

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

One seam that is worth naming, because CI's copy of the breaking check does not
close it: CI reads the **committed** documents, since `check-generated` needs
`sqlc` and is local-only. A field renamed in a Go type but never regenerated
therefore reaches neither check — `make check` catches it as generated-file
drift, and nothing in CI does. That is the pre-existing shape of the sqlc split,
not something the breaking check introduces, and reading the committed file is
right on its own terms: the committed file is what a hub author downloads.

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
- **No hub is contacted.** The runner is tested against `yad hub` in process.
  There is no test against Zumino or yashiki; `yad conformance <url>` (epic E7)
  is how a real hub is checked, by hand.
- **No release is fetched.** `yad upgrade` and `scripts/install.sh` both go
  through `gh`, so both are tested against a `gh` that is a shell stub on
  `PATH` and a release made of files on disk. Nothing here downloads a real
  release, and no check on this machine proves the install script on a fresh
  Linux VM.
- **No breaking-change check has anything to compare against yet.** `oasdiff`
  is wired — `make check-breaking`, in `make check` and in CI — but this
  repository has no `v[0-9]*` tag, so there is no released document to hold the
  current one against. It prints that it is inert on every run, naming what it
  did not do, and starts guarding the moment the owner pushes the first release
  tag. Nothing else has to change then. Until that push, a green
  `check-breaking` is not evidence that either document is compatible with
  anything; it is evidence that nothing has been released.
