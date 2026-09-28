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

Under `claude-2.1.280/`:

| Fixture | The turn |
|---|---|
| `tool-outcomes` | a Bash command that exits 3, one that succeeds, and a `Read` of no file: each result's `is_error` as Claude reports it |

Under `claude-2.1.284/`, recorded for forks (DEV-48, decision 0065):

| Fixture | The turn |
|---|---|
| `fork` | `--resume` of a session asked to remember a word, `--fork-session` and a new `--session-id`: the answer is the word, in the new session; the recorder checked the forked session's transcript was not written to |
| `fork-missing` | the same flags naming a session that does not exist: `No conversation found` for the forked id, under the new one |

A dead process, a truncated or garbled stream, an oversized line and a
mismatched session are not recordable on demand; the tests derive them from
these files (`derive` in `claude_test.go`).

Derived the same way, and written into the version directory rather than made
at test time because the acceptance criteria name them (DEV-27):

| Fixture | The turn |
|---|---|
| `usage-limit-five-hour` | a `rate_limit_event` rejecting the `five_hour` window, then a 429 result |
| `usage-limit-weekly` | the same for `seven_day`, with the five-hour window still nearly empty |

**What in them is recorded and what is not.** Every field of
`rate_limit_info` — `rateLimitType`, `resetsAt`, `status`, and the
`unifiedWindows` map with each window's `utilization` and `resetsAt` — comes
from the events in `plain`, which claude really wrote. The one part not
observed is `status: "rejected"`: see the next section for why it could not be.

## The instrument: recording a limit without exhausting an account

Two fixtures under `claude-2.1.278/` are recorded against a local HTTP server
that authors claude's answers instead of the API (`instrumentServer` in
`record_test.go`, from the DEV-24 instrument). They reach no model, spend no
token and need no login:

| Fixture | The turn |
|---|---|
| `api-retry-then-success` | the API answered 429 once, claude retried by itself, the turn succeeded |
| `retries-exhausted-429` | 429 to all ten of claude's retries: the turn fails with `api_error_status: 429` and no `rate_limit_event` |

Together they are DOMAIN.md's usage-limit/rate-limit distinction in two files:
the first must never mark an account limited, the second must.

**Both are named for what was recorded, not for how the adapter reads it.**
`retries-exhausted-429` says what the stream shows — a 429 that outlasted the
retry ladder. That the adapter then calls it a usage limit is `isUsageLimit`'s
pre-existing rule (decision 0021), not something these files establish.

**The error text in them is the instrument's, not the API's.** The body
`{"type":"error","error":{"type":"rate_limit_error","message":"You've exceeded
your account's rate limit."}}` was authored by `instrumentServer`, so nothing
in these fixtures is evidence about how a real limit words itself. What they
are evidence of is the **frame shape** — how many `api_retry` frames claude
emits, what it puts in the result, and that no `rate_limit_event` appears.
Read them for that and for nothing else.

```bash
YAD_REAL_HARNESS=1 YAD_RECORD_ONLY=retries-exhausted-429 go test -tags realharness -timeout 20m -run TestRecord -v ./internal/adapter/claude/
```

`-timeout 20m` because `retries-exhausted-429` is claude's own retry ladder:
ten attempts with a backoff reaching forty seconds, about three minutes in
which nothing is asked of a model. Go's default test timeout is ten minutes
and the whole recording run would pass it.

**What the instrument cannot record.** Claude 2.1.278 reached over
`ANTHROPIC_BASE_URL` with an `ANTHROPIC_API_KEY` emits no `rate_limit_event`
at all, whatever rate-limit headers the answer carries — the unified windows
belong to a subscription and an API key has none. So the subscription
rejection (`status: "rejected"`) is the one shape here that is inferred rather
than observed, and it is inferred from the `allowed` events in the recorded
fixtures, which are real. Recording it for certain needs a real subscription
that is really out of quota.

To record against a new Claude release:

```bash
YAD_REAL_HARNESS=1 go test -tags realharness -timeout 20m -run TestRecord -v ./internal/adapter/claude/
```

then point `fixtures` in `claude_test.go` at the new directory, and read the
diff before committing it. The two instrument fixtures above are recorded by
the same command and cost nothing; `-timeout 20m` is for them.
