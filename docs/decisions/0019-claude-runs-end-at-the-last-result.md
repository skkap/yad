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
- **Only a final result decides the run.** A result is final when every frame
  written had been taken by the time it came and nothing was queued behind it.
  An interrupt closes input too, but a steer it made Claude drop was not
  answered, so the result it leaves is not final. A result that was not final —
  a steer taken after it, queued behind it, or never read — decides nothing by
  what it says: the run is cancelled if it was interrupted or cancelled, and
  `harness_exited` if Claude simply died. The one exception is an error Claude
  gives before reading any input at all (a resume with no transcript), which
  nothing else will follow.
- **The final result is read by what it says**, never the exit status: an
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
