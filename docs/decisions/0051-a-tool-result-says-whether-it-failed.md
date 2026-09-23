---
date: 2026-09-24
---

# A tool result says whether it failed

A `tool_result` event carried only the call's id, its output and whether that
was cut. Both harnesses know whether a call failed, and yad dropped it: a hub
that wanted to show a failed call as failed — Zumino's Logs tab, ZUM-88 — had
to recognise each harness's wording in the output (Claude's `Exit code N`,
Codex's `[exit N]` tail yad itself appends), which breaks the day either
changes it (DEV-125).

## Decision

`ToolEvent` gains two optional fields, set on a `tool_result`:

- **`is_error`**, a boolean: whether the call failed, as the harness reported
  it. Absent when the harness did not say.
- **`exit_code`**, an integer: a shell command's exit status, where the
  harness reports one as a number.

Neither is ever read out of the output. The output is unchanged, `[exit N]`
and `[declined: …]` tails included.

**Claude Code**: `is_error` is the `tool_result` block's own. The Messages
API defines an absent one as false, and every failure recorded carries it
true — a command that exited non-zero, a `Read` of no file, both calls the
permission mode refused (2.1.276 and 2.1.280) — while some successes leave it
out (a `Read`), so absent is read as false, never as unknown. Claude reports
no exit status as a number — `Exit code 3` is text — so a Claude result has
no `exit_code`.

**Codex**, from the fields the pinned protocol
([0037](0037-a-codex-run-is-its-own-turn-and-its-protocol-is-pinned.md))
gives each item: a `commandExecution` fails when its status is `failed` or
`declined` or its `exitCode` is not 0, succeeds when `completed` with 0, and
carries `exitCode` as `exit_code`; a `fileChange` and a
`collabAgentToolCall` by their status; an `mcpToolCall` by its status or an
`error`; a `dynamicToolCall` by its `success`, else its status. A
`webSearch` or `imageView` has none of these and leaves `is_error` absent, as
does a status yad does not know.

## Why no feature gates it

HUB.md §11: within v1 things are added and both sides tolerate it — unknown
fields are ignored (§2), and conformance checks that a hub takes a body
carrying a field no version defines. What 0047 closed is the enums, because a
value outside one fails a hub's validation where an unknown field does not.
These are new optional properties, not enum values, so a hub generated before
them ignores them, and a hub generated after them reads their absence as the
harness not having said.

## Considered

**A three-valued `outcome` enum** (`succeeded`, `failed`, `declined`) would
have said a declined call apart from a failed one; it is a new v1 enum, which
0047 would put behind a hub feature, and a declined call's output already
says so. **Parsing Claude's `Exit code N`** into `exit_code` is the
pattern-matching this change exists to end.
