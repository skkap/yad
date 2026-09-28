# Codex app-server fixtures

Each `codex-<version>/<name>.jsonl` is one run's whole conversation with
`codex app-server --listen stdio://`: what Codex wrote, line for line, and what
the adapter wrote, wrapped as `{">": …}`. They were recorded by
`record_test.go` through the real adapter, on `gpt-5.6-luna`, with a throwaway
`CODEX_HOME`, and scrubbed: paths become `/work`, `/codex-home` and
`/home/user`, the host name becomes `host`, the installation id and the
ChatGPT account id (in 0.157.1's rate-limit snapshots) are zeroed and the user
agent no longer names the terminal it ran in.

| Fixture | The run |
|---|---|
| `plain` | a one-word answer, with a brief context as developer instructions |
| `tool` | one shell command, then its output as the answer |
| `error` | an unknown model: the turn fails with Codex's own message |
| `tool-outcomes` | a command that exits 3 (`status: failed`, `exitCode: 3`), then one that succeeds |
| `file-change` | a file created with `apply_patch`: a `fileChange` item, `status: completed` |
| `effort` | `plain` with `effort: low` on the `turn/start` |
| `effort-rejected` | an effort the model does not take: the turn fails with Codex's own message |
| `resume-missing` | `thread/resume` of a thread Codex has no rollout for |
| `resume` | a second turn in a thread; Codex replays the first turn's usage before it |
| `resume-context` | a second turn whose context the first did not have, injected before it as a developer message |
| `interrupt` | `turn/interrupt` sent at the first text |
| `steer` | `turn/steer` sent at the first tool call, answered in the same turn |
| `approval` | `approval = "untrusted"`, `sandbox = "read-only"`: a command approval, declined |

Recorded by `TestRecordModels` the same way, with no thread, so spending
nothing (DEV-50):

| Fixture | The conversation |
|---|---|
| `list-models` | `initialize`, then `model/list`: the recording login's models |

Written by hand from `plain`, because they cannot be recorded without
exhausting an account or a context window:

| Fixture | The run |
|---|---|
| `usage-limit` | the turn fails `usageLimitExceeded` after a snapshot with the primary window full |
| `usage-limit-weekly` | `usage-limit` with the secondary (10080-minute) window full instead, and the primary at 58% |
| `usage-limit-unnamed` | the same with no snapshot: the adapter asks `account/rateLimits/read` |
| `prompt-too-long` | the turn fails `contextWindowExceeded` |

Written by hand from the schema: the device-code login a hub starts
(DEV-135). Recording one needs a person to type the code at OpenAI, so until a
login is recorded by hand on a machine with Codex, the order of the
notifications after a code is typed — and whether `auth.json` is written
before `account/login/completed` — is what the schema suggests, not what Codex
was seen to do. A start and its cancel were measured on 0.157.1, in a
throwaway `CODEX_HOME`: `login-device-cancel` is what Codex said, with the
login id and code replaced by the ones every login fixture uses. The handshake
is `plain`'s; Codex's other error texts are invented, since the runner never
reads them.

| Fixture | The login |
|---|---|
| `login-device` | a link and a code, then `account/login/completed` with `success: true` and `account/updated` |
| `login-device-refused` | the same, ending `success: false` — a ChatGPT workspace with device-code login off |
| `login-device-cancel` | `account/login/cancel` while the code is out: `canceled`, then a completion with `success: false` |
| `login-device-unsupported` | a codex whose `account/login/start` has no `chatgptDeviceCode`, answering with an error |

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
committing it. The hand-written fixtures are carried over from the previous
release's, on the new `plain`'s handshake and in the new release's shapes.

Every release in `pinned` keeps its directory: `yad doctor` calls each of them
ready, so `TestEveryPinnedReleaseReplays` replays each one's recordings, and
`TestPinnedSchema` checks each one's pin. Dropping a release from `pinned` is
what lets its directory go.
