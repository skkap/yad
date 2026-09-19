---
date: 2026-09-19
---

# A Codex run is its own turn, ended by turn/completed, against a pinned protocol

What `codex app-server` 0.147.0 does over stdio, recorded while the adapter was
built (DEV-20, DEV-21, DEV-22), and what the adapter does about it. The contract
it gives the executor is the Claude adapter's
([0021](0021-claude-runs-end-at-the-last-result.md),
[0031](0031-a-failed-resume-is-the-hubs-to-decide.md)): events from the
harness's own words, an outcome only from its own answer, the native id pinned
as soon as it exists.

- **One process and one turn per run.** `initialize`, `initialized`, then
  `thread/start` — or `thread/resume` with the stored thread id — then one
  `turn/start` with the instruction. The brief's context is the thread's
  `developerInstructions`, Codex's equivalent of an appended system prompt.
  Each of those requests must be answered within 30 s. The handshake happens
  after `Start` returns, so everything it can end in is an outcome with its
  class, never a start error.
- **The thread id is the native session id.** Codex chooses it; the adapter
  exposes it the moment `thread/start` answers, before the first event after
  it, so the executor pins it before anything else can happen.
- **Only the run's turn is the run's.** Codex writes subagents' threads to the
  same pipe, and a resume replays the thread's earlier usage as notifications.
  Notifications naming another thread are dropped, and nothing turn-scoped is
  read until the run's own turn has started — its `turn/started`, the first
  after `turn/start` was sent, or `turn/start`'s answer, whichever comes first
  — and then only what names that turn.
- **Only `turn/completed` decides the run.** `completed` is a success, with the
  last agent message as the final text (the last one marked `final_answer`,
  when commentary follows it). `failed` is read by `codexErrorInfo`:
  `usageLimitExceeded` is `usage_limit`, `contextWindowExceeded` is
  `prompt_too_long`, anything else `harness_error` with Codex's message (the
  API's JSON body unwrapped). `interrupted` is `cancelled` when YAD asked, and
  `harness_error` when it did not. An app-server that exits — 0 or not —
  without it is `harness_exited`, or `cancelled` if YAD had asked it to stop.
  A turn that completed before an interrupt reached it stands, as decision
  [0025](0025-a-cancel-is-repeated-and-an-answer-that-landed-stands.md) has it.
- **A usage limit carries its window.** Codex publishes the account's windows
  in `account/rateLimits/updated`; the limit is the window at 100% (the later
  reset when both are), named as Codex names it, `primary` or `secondary`. When
  no snapshot came before the failure, the adapter asks
  `account/rateLimits/read` once, for 5 s, before it closes the conversation.
  E6 builds failover and waiting on this.
- **A failed resume is the hub's.** `thread/resume` answered `no rollout found`
  is `session_not_found`, which the executor reports as `resume_rejected`. A
  resume that answers with a different thread is `session_mismatch`, and the
  run stops before any turn runs in it. Neither moves the stored id.
- **Usage is this turn's.** The first `thread/tokenUsage/updated` of the run's
  turn counts its `last`, every later one the growth of `total`; a replayed
  total is never charged. Codex counts cached input inside input; the event
  keeps them apart, as Claude's does. Codex gives no cost.
- **Steer is `turn/steer` into the same turn**, which answers once, having
  read it. Codex accepts or refuses it before the turn goes on, so `Steer`
  waits (up to 10 s) and a refusal reaches the hub as `steer_failed`. A steer
  that arrives before the turn has an id waits for it.
- **Interrupt is `turn/interrupt`, and nothing waits on its answer**: Codex
  answers only while a turn is live, and the `turn/completed` that follows is
  the answer that matters. One that arrives before the turn has an id is sent
  the moment it has one; one before `turn/start` ends the run with nothing run.
- **Once the turn is over, input closes** and the app-server exits; it gets
  2 s, then its process group is stopped.
- **The protocol is pinned by schema, as a warning.** The adapter's slice of
  `codex app-server generate-json-schema` — the methods it sends or reads, the
  responses to them, and every definition they reach, descriptions and titles
  dropped — is hashed and pinned per recorded version
  (`internal/adapter/codex/schema.go`, the bundle in its testdata). The runner
  computes it for the installed codex once per binary and version; a hash it
  does not know becomes a warning on the harness in the capability document
  (`warnings`, a v1 addition) and in `yad doctor`. It does not make Codex
  undrivable.

## Considered options

**Hash the whole schema.** Simpler, and it moves with every weekly release —
a new method, a reworded description — so the warning would always be on and
would be ignored. The slice moves only when something the adapter reads does.

**Refuse runs on drift.** Safer in principle, and it would take a working
runner out of service for a change in a part of a notification the adapter
never reads. The warning tells the owner and the hub; the recorded fixtures
say what was verified.

**Report steer as refused before the turn starts.** Simpler, and a steer sent
in the first second of a run would fail for no reason the sender could see.

**Wait for `turn/interrupt`'s answer.** Codex never answers one that arrives
after the turn ended, so an interrupt racing the end of a turn would hang the
runner's event loop.
