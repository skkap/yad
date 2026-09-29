# OpenCode ACP fixtures

Each `opencode-<version>/<name>.jsonl` is one run's whole conversation with
`opencode acp`: what OpenCode wrote, line for line, and what the adapter
wrote, wrapped as `{">": …}`. They were recorded by `record_test.go` through
the real adapter, with OpenCode 1.18.33 installed in a throwaway npm prefix
(`npm i --prefix <dir> opencode-ai@1.18.33`) and run in a throwaway home — its
XDG directories and HOME — on OpenCode Zen's free models, with no account and
no spend. Paths are scrubbed to `/work` and `/opencode-home`; nothing else
names the machine. Only the latest recorded release is kept (decision 0067).

| Fixture | The run |
|---|---|
| `plain` | a one-word answer on `opencode/nemotron-3.5-lightning-free`, with a context |
| `tool` | one bash call, `cat note.txt`, then its output as the answer |
| `tool-outcomes` | a command that exits 3 (`metadata.exit: 3`), then one that succeeds |
| `file-change` | a file created with the write tool |
| `error` | a model OpenCode does not have: `session/set_config_option` refused |
| `effort` | `effort: high` set on `opencode/longcat-2.5-preview-free`, which has levels |
| `effort-rejected` | an effort the model does not have: refused |
| `context` | the context names a codeword, and the answer gives it: the instruction route reaches the model (decision 0050, 0073) |
| `resume-missing` | `session/resume` of a session that does not exist, and the `session/list` of every session that tells it apart |
| `resume` | a second turn in a session, resumed without a replay |
| `resume-context` | a second turn whose context the first did not have, answered from it |
| `fork` | `session/fork` of a session asked to remember a word, from a workdir of the fork's own (`/fork-work`), the replay before its answer, then a turn in the new session answering with the word |
| `fork-missing` | `session/fork` of a session that does not exist |
| `interrupt` | `session/cancel` sent at the first text; the prompt answers `cancelled` |
| `permission` | `bash` set to ask (`OPENCODE_CONFIG_CONTENT`), answered `allow_once` |
| `permission-rejected` | the same, with `permission_mode = "reject"`, answered `reject_once` |
| `provider-error` | recorded while Zen's upstream for `ling-3.0-flash-fin-free` was down: `-32603`, `errorName: APIError`, OpenCode's words |

`list-models.txt` is what `opencode models` printed in the same throwaway
home: Zen's free models.

Written by hand from `plain`, its answer replaced by the error OpenCode
1.18.33 sends, because none can be recorded without exhausting or breaking a
login:

| Fixture | The run |
|---|---|
| `usage-limit` | `-32603`, `errorName: APIError`, Zen's `5-hour usage limit reached. Resets in 2hr 30min.` |
| `auth-rejected` | `-32000`, authentication required, `providerId: opencode` |
| `prompt-too-long` | `-32603`, `errorName: ContextOverflowError` |
