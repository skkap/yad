---
date: 2026-09-19
---

# A Claude run ends at the last result, and only a result decides it

What `claude -p` 2.1.276 actually does over stream-json, recorded while the
adapter was built, and what the adapter does about it:

- **Steer is supported, as a queued user frame.** A `user` frame written
  mid-turn is read at the next tool boundary, and the turn carries on with it.
  If the turn has no tool boundary left, Claude finishes, emits a `result`, and
  answers the frame as a follow-up turn with its own `result` — in the same
  process, so the same run. The adapter passes `--replay-user-messages`, which
  echoes each frame when Claude takes it, and closes stdin only once every frame
  it wrote has been echoed and a result has followed. `queued_turn_count` alone
  is not enough: a frame written just before a result is not yet counted in it.
- **The run's outcome comes from the last result**, never the exit status: an
  error arrives as `subtype: success` with `is_error: true` (a bad model,
  `prompt_too_long`), and Claude exits 1 after an interrupt or an error. The
  classes are `prompt_too_long` (from `terminal_reason`), `usage_limit`
  (a `rate_limit_event` with `status: rejected`, a 429, or the older
  `usage limit reached|<epoch>` text), `session_not_found` (a `--resume` with no
  transcript), `session_mismatch`, `harness_error`, and `harness_exited` when the
  stream ends with no result at all.
- **Usage is the last result's `modelUsage`.** It is cumulative for the
  process, so it covers follow-up turns too.
- **An interrupt** is a `control_request` with `subtype: interrupt`; Claude
  answers it, drops any queued frames, and ends with an
  `error_during_execution` result, which the adapter reports as cancelled. A
  turn that succeeded before the interrupt landed is reported as succeeded.
- **A permission prompt that reaches the adapter is denied.** In `-p` mode
  Claude denies unpermitted tools itself (`system/permission_denied`); a
  `can_use_tool` request can still arrive, and nobody is there to answer it.
- **Text is streamed in chunks**, flushed every second or 4 KiB, not one event
  per delta and not one per finished message. Thinking is not streamed by
  Claude; its start becomes a `thinking` status, which is what the inactivity
  watchdog needs.
- **The adapter chooses the session id** for a new session (a UUID v4) and
  exposes it from the moment the turn starts, so the runner can pin it before
  the turn ends.

## Considered options

**Report steer unsupported** — simpler, and wrong: Claude does take the input
mid-turn when it can. **Close stdin at the first result** — loses a steer
written moments before it. **One event per delta** — thousands per answer, and
the hub stores every one.
