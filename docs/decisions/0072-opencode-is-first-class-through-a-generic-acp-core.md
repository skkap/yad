---
date: 2026-09-30
---

# OpenCode is first-class through a generic ACP core

The long tail of coding-agent CLIs — Gemini CLI, GitHub Copilot CLI, Cursor,
OpenCode — was recognised and never driven. Most of them now speak the Agent
Client Protocol (ACP): JSON-RPC over the agent's stdin and stdout, one object
per line, the protocol Zed and JetBrains drive agents with. One adapter for
the protocol, and a small description per agent, promotes each one that
speaks it natively. Not Claude or Codex, which keep their own adapters
([0006](0006-claude-by-stream-json-codex-by-app-server.md)): each native
protocol carries what ACP lacks — steer, usage windows, a system prompt.

The research of 2026-09-30 (on DEV-44) compared the four; the owner decided
OpenCode first, Copilot next, Gemini and Cursor not now. DEV-44.

## Decision

**A generic ACP v1 core, `internal/adapter/acp`, hand-written.** Version 1
(`protocolVersion: 1`, schema release 1.23.0): v2 is in alpha, answers a
prompt when it accepts it rather than when the turn ends, and drops
`session/load` and modes. An agent that answers `initialize` with another
version is not driven. The JSON-RPC client is Codex's, moved to
`internal/adapter/jsonrpc` for both to share; no dependency is added (the Go
SDK lags the schema). The core sends `initialize` (no filesystem, no
terminal: the agent works in the workdir with its own tools), then
`session/new`, `session/resume` or `session/fork`, then
`session/set_config_option` for the run's model and effort, then one
`session/prompt`, whose answer ends the run. Only updates naming the run's
session, and only once its prompt is sent, are the run's: a fork's replay of
the conversation it copies comes before its answer, and subagents write their
own sessions to the same pipe.

**What is the agent's is an `acp.Agent`**: how to start it, what its run's
environment is, how its failures read, what its login check is. OpenCode's is
`internal/adapter/opencode`.

**A run maps onto ACP like this.**

- *Resume* is `session/resume`, which answers without replaying the
  conversation; `session/load` would replay all of it. A resume or a fork
  the agent refuses is asked once more by `session/list`, every page of it and not narrowed to
  the workdir: an id no page names is `session_not_found`, since OpenCode
  1.18.33 refuses a missing session with a bare `-32603 OpenCode service
  failure` that a runner cannot tell from any other. Past fifty pages the
  refusal is reported as it is.
- *Fork* is `session/fork` (unstable in v1, and in OpenCode's capabilities):
  a new session holding a copy, which the fork's run then prompts
  ([0065](0065-a-fork-is-a-new-session-opened-from-another-sessions-conversation.md)).
  The fork's `cwd` is its own workdir, not the one the session it forks was
  made in; measured on OpenCode 1.18.33 between two plain directories, and
  recorded that way (`fork`). A `session/list` narrowed to the fork's
  workdir does not name the session it forked, which is why the check above
  is not narrowed. Between two git repositories OpenCode keeps sessions per
  project, and a fork across them was not measured.
- *Model and effort* are the session's config options of ACP's categories
  `model` and `thought_level`, each set by the agent's own option id; the
  words are the agent's, checked by the agent
  ([0049](0049-a-runs-effort-is-the-harnesss-word.md)). A model with no
  `thought_level` option refuses a run carrying an effort, before anything is
  asked. An effort is set on every run that has one, even when the option
  already shows it: the value shown can be the default the agent would pick.
- *Interrupt* is `session/cancel`; the prompt then answers `cancelled`. The
  prompt is queued before an interrupt can see it was sent, so a cancel never
  reaches the agent ahead of the turn it is for.
- *Steer* does not exist in ACP v1. The adapter says so
  (`adapter.Steerer`), so the runner lists no `steer` for the harness
  ([0069](0069-a-per-run-feature-is-its-harnesss.md)) and a hub reading the
  lists sends none; a steer that arrives anyway fails with its reason.
- *Usage* is the prompt answer's `usage` (unstable in v1). OpenCode 1.18.33
  reports the turn's last model call there, not the sum of its calls, and
  its cost only as the session's running total in `usage_update`; the run's
  usage is that last call, reasoning counted as output, and no cost.
- *Permission requests* (`session/request_permission`) are answered from the
  owner's `permission_mode` under the harness in config.toml: `allow`, the
  default, for the reason 0036 gives Codex's approval policy, or `reject`.
  One-time options are chosen over standing ones, so a change to the setting
  holds from the next request. What the agent's own configuration denies is
  never asked, and stays denied.

## OpenCode

**The pin is the release.** OpenCode cannot print the ACP surface it speaks,
as Codex prints its schema, so the adapter is pinned to the release its
fixtures were recorded on, 1.18.33, and the core to the schema release it was
written against, whose surface `schema_test.go` hashes. Another OpenCode is
driven with a warning in the capability document and `yad doctor`, as an
unpinned Codex is; pinning a new one replaces the recordings
([0067](0067-only-the-latest-recorded-harness-protocol-is-pinned.md)).

**Its server is password-protected, per run.** `opencode acp` serves the
agent from an HTTP server it opens on 127.0.0.1 at a random port, and talks
to itself over it; unprotected, anything on the machine that finds the port
drives the session with the run's credentials. Every run sets
`OPENCODE_SERVER_PASSWORD` to 32 random bytes, hex encoded, in the child's
environment only — never in argv, a log, an event or a file. **The runner
still listens on no port ([0004](0004-runner-listens-on-no-port.md)); this
child does**, on loopback, behind that password, for as long as its run.

**The run's context is an instruction file.** ACP has no system-prompt field.
OpenCode reads `config.instructions` into the system prompt of every request
it makes, so the context is written to a `0600` file in a directory of the
run's own, named in inline configuration (`OPENCODE_CONFIG_CONTENT`, merged
over the owner's own if their environment sets one) and removed when OpenCode
exits. It is outside the conversation, survives a compaction, and reaches a
resumed or forked session's turn as it reaches a new one's
([0050](0050-a-runs-context-reaches-the-harness-on-every-run.md)). Measured on
1.18.33 with `opencode/nemotron-3.5-lightning-free`: a new session told
nothing but the instruction `The codeword is BLUE`, asked for the codeword,
answered `BLUE`, its reasoning quoting "the system prompt"; so did a resumed
session whose first run had no context, and a fork (`context`,
`resume-context` and `fork` in the fixtures).

**OpenCode's own variables are the owner's.** Every `OPENCODE_*` in a run's
environment as the runner hands it — which carries the hub's grants — is
dropped: `OPENCODE_PERMISSION`, inline configuration that can name a
provider and its key, `OPENCODE_AUTH_CONTENT`. A hub cannot set or widen what
a harness may do (AGENTS.md), as Claude's adapter drops `IS_SANDBOX`. The
owner's own, in the runner's environment, reach OpenCode as they would at a
terminal.

**OpenCode runs on its own login, checked as an account is**
([0053](0053-a-harness-on-its-own-login-is-checked-like-an-account.md)). It
has no login check of its own, and on OpenCode Zen's free models needs no
credential at all, so its check is its model list: `opencode models` naming a
model is logged in, naming none is not — the owner runs `opencode auth login`
— and failing is no answer, a warning. Per-account OpenCode homes are out of
scope: `yad account add opencode` says accounts are for Claude and Codex, and
a hub login is refused the same way.

**Its models are `opencode models`**, which lists what its providers offer
without a session and spends nothing; ACP's own model option exists only on
a session, which OpenCode would keep.

**Failures are classified from its structure first.** A prompt OpenCode
refuses carries an ACP code and, beside it, `errorName`:

- `-32000` (authentication required, from `ProviderAuthError`) is the
  credential refused: `harness_error`, and the runner told the provider
  refused it (`Outcome.AuthRejected`).
- `ContextOverflowError` is `prompt_too_long`.
- A usage limit is said only in words: OpenCode passes OpenCode Zen's message
  through as the error's message. The wording of 1.18.33's console —
  `5-hour|Weekly|Monthly usage limit reached. Resets in <time>.` and
  `Subscription quota exceeded. Retry in <time>.` — is matched only as the
  whole start of the message, and its `<time>` (`2 days`, `3hr 20min`,
  `45min`) is the reset. A rate limit (`Rate limit exceeded. Please try again
  later.`, Zen's free tier) is not a usage limit (DOMAIN.md).
- Everything else is `harness_error` with OpenCode's words.

**Known from the source, not measured:** OpenCode retries a 429 by itself,
sleeping the `retry-after` it was given — up to days for a weekly window —
without saying so over ACP. A run on an exhausted OpenCode Go window may
therefore sit silent until the runner's inactivity watchdog ends it, rather
than fail `usage_limit`. Nothing in ACP v1 lets the adapter see that wait.

**Recorded on free models, spending nothing.** The fixtures under
`testdata/opencode-1.18.33/` were recorded through the real adapter with
OpenCode installed in a throwaway npm prefix and running in a throwaway home,
on OpenCode Zen's free models, with no account: the usage limit, the refused
credential and the context overflow are written by hand from `plain`, since
none can be recorded for free.

## Considered options

**The Go ACP SDK.** A dependency on every runner for a few hundred lines,
and it lagged the schema by months at the time of the research.

**`session/load` for a resume.** Replays the whole conversation as updates
before it answers, each of which would have to be filtered out, on every
continuing run.

**The context as a first content block** with `audience: ["assistant"]`,
which OpenCode sends to the model as a hidden user part. It is inside the
conversation: a compaction summarises it, and a resumed session would read
every earlier run's context too.

**Model listing through `session/new`.** Creates a session OpenCode keeps,
once per login per hour.

**Gemini, Cursor and Copilot in the same change.** Each needs its own
recordings and its own answers — Copilot's keychain-bound login, Gemini's
end of subscription use — and none of them forks or steers; the core is
built so each is a small change on its own ticket.
