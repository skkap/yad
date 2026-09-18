---
date: 2026-09-18
---

# Sessions map to the harness's own, and never wrap them

YAD stores `session → (harness, native id, workdir, account)` and hands the
native id back to the harness. It never stores, replays or reimplements
conversation state: the harness's transcript is the conversation. Everything a
harness learns about continuing a conversation — compaction, forks, tool state —
YAD gets for free and cannot get wrong.

A session runs **one harness process per run** by default. A **live** mode, one
process kept across runs for conversational use, is reserved in the protocol and
advertised as a capability, but not built: it would make YAD own idle processes
and their memory, and it can arrive later without changing anything else.

## Considered options

**YAD owns the conversation**, sending history each time. Harness-independent,
and a reimplementation of exactly the part the harnesses do best.
