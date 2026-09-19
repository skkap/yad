---
date: 2026-09-19
---

# A failed resume ends the run, names why, and leaves the session to the hub

A run that continues a session hands the harness the native id the session
store holds — `--resume <native id>` for Claude. DEV-15 settles what the hub
sees when that does not work, and when the pointer is written.

**The pointer is written at spawn.** Claude's session id is chosen by YAD
(decision [0021](0021-claude-runs-end-at-the-last-result.md)), so the executor
records it the moment the harness is started, before its first line; every
event after re-checks it, for an adapter that learns its id later (Codex's
thread id). A runner that dies between spawn and the first event still knows
what to resume.

**Two classes, because a hub does two different things about them.**

- `resume_rejected` — the run continued a session and the harness had no
  conversation to continue: Claude's `No conversation found`, which the
  adapter names `session_not_found`. The transcript is gone — deleted,
  another machine's, a first run killed before Claude wrote it. Nothing on
  this runner can bring the context back; the hub starts a new session, or
  tells whoever asked that the conversation is lost. The executor renames the
  class in the result and in the error event before it, so the stream and the
  result agree; `session_not_found` on a run that resumed nothing stays as it
  is, since it would be a different fault.
- `session_mismatch` — the harness ran, but under another session id than the
  one it was given: the resume silently did not take, and whatever the turn
  did, it did without the conversation. It surfaces as its own class, not as
  `resume_rejected`: the run may have changed files in the workdir, and a hub
  that treats it as "nothing happened, start over" would be wrong. The adapter
  interrupts the turn as soon as it sees the echoed id.

**The session is left as it was.** Neither failure moves the resume pointer or
closes the session. After a mismatch the stored id is still the one YAD chose,
never the one the harness echoed: adopting it would continue a conversation
that lacks everything before this run, which is the silent failure the class
exists to expose. Whether the session is worth another try, or closed, is the
hub's call; closing is `close_session` (decision
[0011](0011-hub-closes-sessions-runner-collects.md)), and a new run in a
session whose transcript is gone fails `resume_rejected` again, at the cost of
a process start and no tokens.

## Considered options

**Report both as `resume_rejected`.** One class fewer, and a hub could not
tell a turn that never ran from one that ran in the wrong conversation.

**Fall back to a new conversation on a rejected resume.** The run would
succeed, answering without the context the hub sent it to use — the exact
failure sessions exist to prevent (decision
[0007](0007-sessions-map-never-wrap.md)). The hub knows whether a fresh start
is acceptable; the runner does not.

**Adopt the echoed id after a mismatch**, so the next run at least continues
something. It would continue the wrong thing without saying so.
