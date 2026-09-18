# Local checks

What to run before pushing. This file is the authority — `fly` and anything else
that ships work reads it here and runs nothing else.

YAD is a single Go module with no dependencies outside the standard library, so
there is no install step and no lockfile to agree with. The whole bar is three
commands, and `make check` is exactly those three in that order.

## Module
Setup: none
Timeout: 3m

```bash
gofmt -l .        # must print nothing
go vet ./...
go test ./...
go build ./...
```

`gofmt -l .` printing a path is a failure even though it exits 0 — `make lint`
turns that into a non-zero exit, which is why CI runs `make check` rather than
the bare commands.

## Cross-compilation
Timeout: 2m

```bash
make dist
```

The runner is developed on macOS and deployed on Linux, so a build that only
works on the laptop is a broken build. Anything reaching for `syscall` or a
`_darwin.go` file gets caught here and nowhere else.

## What CI does not run

- **No agent is invoked.** Detection is tested against an empty `PATH`; nothing
  in the suite spends a token or needs `claude` on the box.
- **No control plane is contacted.** There is no integration test against Zumino
  or yashiki, and there will not be one until the protocol in `DESIGN.md` is
  real.
