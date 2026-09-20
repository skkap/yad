# Codex app-server fixtures

Each `codex-<version>/<name>.jsonl` is one run's whole conversation with
`codex app-server --listen stdio://`: what Codex wrote, line for line, and what
the adapter wrote, wrapped as `{">": …}`. They were recorded by
`record_test.go` through the real adapter, on `gpt-5.6-luna`, with a throwaway
`CODEX_HOME`, and scrubbed: paths become `/work`, `/codex-home` and
`/home/user`, the host name becomes `host`, the installation id is zeroed and
the user agent no longer names the terminal it ran in.

| Fixture | The run |
|---|---|
| `plain` | a one-word answer, with a brief context as developer instructions |
| `tool` | one shell command, then its output as the answer |
| `error` | an unknown model: the turn fails with Codex's own message |
| `resume-missing` | `thread/resume` of a thread Codex has no rollout for |
| `resume` | a second turn in a thread; Codex replays the first turn's usage before it |
| `interrupt` | `turn/interrupt` sent at the first text |
| `steer` | `turn/steer` sent at the first tool call, answered in the same turn |
| `approval` | `approval = "untrusted"`, `sandbox = "read-only"`: a command approval, declined |

Written by hand from `plain`, because they cannot be recorded without
exhausting an account or a context window:

| Fixture | The run |
|---|---|
| `usage-limit` | the turn fails `usageLimitExceeded` after a snapshot with the primary window full |
| `usage-limit-weekly` | `usage-limit` with the secondary (10080-minute) window full instead, and the primary at 58% |
| `usage-limit-unnamed` | the same with no snapshot: the adapter asks `account/rateLimits/read` |
| `prompt-too-long` | the turn fails `contextWindowExceeded` |

`codex_app_server_protocol.schemas.json` is what `codex app-server
generate-json-schema` wrote for that version. `schema.go` pins the hash of the
part the adapter uses (decision 0037); `schema_test.go` checks the pin against
this file.

To record against a new Codex release:

```bash
YAD_REAL_HARNESS=1 go test -tags realharness -run 'TestRecord' -v ./internal/adapter/codex/
```

add the hash `TestRecordSchema` prints to `pinned` in `schema.go`, point
`fixtures` in `codex_test.go` at the new directory, and read the diff before
committing it.
