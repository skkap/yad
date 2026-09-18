# Local checks

What to run before pushing. This file is the authority — `fly` and anything else
that ships work reads it here and runs nothing else.

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
make generate && git diff --exit-code internal/store/db protocol/v1/openapi.yaml
GOOS=… GOARCH=… go build ./...   # linux/amd64, linux/arm64, darwin/arm64, darwin/amd64
```

**Generated files.** A change to `internal/store/*.sql` or to a `protocol/v1`
type needs `make generate` and the regenerated files in the same commit. The
check also fails on a generated file that exists but was never added — the
usual way a new sqlc output file goes missing from a PR.

**The protocol is a public surface.** A diff in `protocol/v1/openapi.yaml` is
a change every TypeScript hub will feel. If it renames or removes anything, it
belongs in v2, not in this PR.

**Cross-compilation** is part of the bar because the runner is developed on
macOS and deployed on Linux: anything reaching for `syscall` outside a `unix`
build tag, or for cgo, is caught here and nowhere else. Everything builds with
`CGO_ENABLED=0`; only the race detector turns cgo back on, for tests.

## What CI does not run

- **No harness is invoked.** Detection is tested against an empty `PATH`,
  children are the test binary re-executed as a fake, and adapters replay
  recorded fixtures. Nothing in the suite spends a token or needs `claude` or
  `codex` on the box. Tests against real harnesses sit behind the `realharness`
  build tag and `YAD_REAL_HARNESS=1`, and are run by hand when recording new
  fixtures.
- **No hub is contacted.** The runner is tested against `yad hub` in process.
  There is no test against Zumino or yashiki; `yad conformance <url>` (epic E7)
  is how a real hub is checked, by hand.
- **No breaking-change check against a released spec yet.** There is no
  released v1 to compare with; `oasdiff` joins the bar with the first release
  (epic E7).
