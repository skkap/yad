# Claude stream fixtures

Each `claude-<version>/<name>.jsonl` is what `claude -p` wrote to stdout for one
short haiku turn, recorded by `record_test.go` through the real adapter and
scrubbed: paths become `/work` and `/home/user`, the init frame's inventory of
the recording machine is dropped, thinking signatures are emptied, and an
oversized echoed prompt is elided.

| Fixture | The turn |
|---|---|
| `plain` | a one-word answer, with a brief context |
| `tool` | one `Read` of a small file, then its contents |
| `error` | an unknown model: an error result that says `success` |
| `prompt-too-long` | a first message over the context window |
| `resume-missing` | `--resume` of a session that does not exist |
| `interrupt` | an interrupt sent at the first text |
| `steer-tool` | a steer taken at a tool boundary: one result |
| `steer-followup` | a steer after the last tool boundary: two results |
| `permission-denied` | `--permission-mode default`, a tool Claude denies itself |

The usage limit, a dead process, a truncated or garbled stream, an oversized
line and a mismatched session are not recordable on demand; the tests derive
them from these files (`derive` in `claude_test.go`).

To record against a new Claude release:

```bash
YAD_REAL_HARNESS=1 go test -tags realharness -run TestRecord -v ./internal/adapter/claude/
```

then point `fixtures` in `claude_test.go` at the new directory, and read the
diff before committing it.
