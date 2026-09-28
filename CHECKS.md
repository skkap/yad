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

Keep the `.sql` query files ASCII. sqlc 1.31.1 computes byte offsets over runes,
so one non-ASCII character anywhere in a query file — an em dash in a comment is
enough — truncates that query and every one after it. `make generate` then
fails naming queries nowhere near the cause.

## Module
Setup: none — needs `sqlc` 1.31.1 and `shellcheck` on PATH (`brew install sqlc shellcheck`)
Timeout: 6m

```bash
make check
```

which is, in order:

```bash
gofmt -l .                       # must print nothing — make lint turns output into a failure
go vet ./...
go tool staticcheck ./...        # pinned in go.mod as a tool, so everyone runs the same version
shellcheck -x scripts/*.sh machines/…   # the install script and the work-machine kit are shell a stranger runs
CGO_ENABLED=1 go test -race ./...
go build ./cmd/yad
make check-generated             # make check-openapi, then sqlc: git diff --exit-code over every generated file
scripts/breaking.sh              # oasdiff, both OpenAPI documents, against the release before this commit
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
machine rather than by eye. It checks each document against the same document
at the release before this commit, and fails on anything oasdiff rates
breaking. For a commit that is itself a release, "the release before" means
the tag just below its own in version order. For any other commit it means
the newest release it builds on. That keeps the check honest during a
release. `release.yml` runs `make check` with HEAD detached at the tag being
published, and if the commit's own tag were the baseline, the check would
compare a file with itself. `scripts/breaking_test.go` runs every release
topology against the script. It covers
`protocol/hubapi/openapi.yaml` too — a service generates its client from that
file alone ([0022](docs/decisions/0022-hub-service-api-beside-the-protocol.md)),
and the same renamed field showed up as one error in the protocol document and
six in the hub's. v0.1.0 is the first baseline. A commit with no earlier
release, such as one older than v0.1.0, has no baseline, and the check says
so rather than passing quietly.

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
make check-openapi      # regenerate both OpenAPI documents, fail on any diff — no sqlc
go vet ./...            # type-checks the tests too, which a build never does
go build ./cmd/yad
go test ./...           # no -race
make check-breaking     # oasdiff, against the documents check-openapi has just proved current
```

and then `govulncheck ./...`, pinned in `ci.yml`.

That is the part whose answer can differ on Linux, plus four deliberate
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

`make check-openapi` is the third, and it exists for the second. The breaking
check reads the **committed** documents, which is right on its own terms —
the committed file is what a hub author downloads. But a `protocol/v1` field
renamed in Go and never regenerated leaves the committed document unchanged,
and oasdiff then correctly reports no break in a document that no longer
describes the code. `check-openapi` is the OpenAPI half of `check-generated`:
`go run ./internal/hub/cmd/openapigen` over both documents and `git diff` over
the two paths, naming the stale file and showing the diff. It needs no `sqlc`,
which is what keeps it out of the dependency that took `check-generated` out of
CI. The suite's `TestOpenAPIIsCurrent` and `TestServiceOpenAPIIsCurrent` catch
the same drift in `go test ./...`; the make target is the gate named for it,
runs first, and does not depend on a test file surviving a refactor.

`govulncheck` is the fourth, and the only check whose answer changes without
a commit: a vulnerability published against a module already here, or against
the Go standard library, turns a green tree red overnight. So it runs in CI on
every push and again every Monday on a schedule, where the vulnerability
database is fetched fresh. It is not in `make check`, because it needs the
network and its answer is a fact about today rather than about the branch.

**CI does not run** `gofmt`, `staticcheck`, `check-generated`'s sqlc half, the
cross-compile matrix, or the race detector. Not because they do not matter —
they are in `make check` and `make check` is the bar — but because they answer
the same on any machine, and paying a hosted runner to repeat a local answer
buys nothing. `-race` is the exception that is about cost rather than
duplication: it needs cgo and roughly triples the suite.

So, concretely, **these reach master only if someone ran `make check`**: a
formatting slip, a staticcheck finding, stale or uncommitted sqlc output,
a darwin-only symbol that breaks the linux build, and a data race. `fly` runs
`make check` before it pushes, which is what makes that safe. A stale OpenAPI
document is not on that list: CI's `check-openapi` fails it by name.

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
- **No release is checked for order until it is pushed.** `release.yml` first
  runs `scripts/release-guard.sh`. It refuses a tag on a commit that an
  earlier release already contains, because that would publish older code
  under a newer version. It runs only on the tag push. Nothing local stops such
  a tag from being made, and the guard is tested against throwaway
  repositories in `scripts/release_guard_test.go`. A tag push runs the
  `release.yml` of the tagged commit, so a commit older than the guard,
  v0.1.0 included, publishes without it. Before tagging one, create the tag
  locally and run `scripts/release-guard.sh <tag>` from a current master
  checkout.
