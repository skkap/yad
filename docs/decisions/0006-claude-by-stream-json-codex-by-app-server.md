---
date: 2026-09-18
---

# Claude is driven by stream-json both ways, Codex by app-server

Claude: `claude -p --input-format stream-json --output-format stream-json
--verbose --include-partial-messages`, a YAD-chosen `--session-id` on the first
run and `--resume` after, stdin held open for the control protocol (`interrupt`,
permission replies). Codex: `codex app-server` over stdio JSON-RPC — `thread/start`
or `thread/resume`, `turn/start`, `turn/interrupt`, `turn/steer` — checked against
the schema `codex app-server generate-json-schema` emits for the installed
version. These are the only surfaces with a real interrupt and mid-turn input,
and Multica, Paseo, vibe-kanban and happy all converged on exactly this pair.

`--include-partial-messages` is not optional: without it a healthy Claude turn
can be silent for minutes, and the inactivity watchdog cannot tell it from a
wedged one (yashiki measured 175 s).

## Considered options

**`codex exec --json`** — simpler, but cancel is kill-only, there is no steer and
no cost, so it would be rewritten before yashiki could use it. **ACP for both** —
one generic adapter, but Claude and Codex speak it only through Node adapters
stacked on their SDKs, losing caller-chosen session ids, fork, steer and sandbox
policy. ACP is the right answer for the long tail of recognised harnesses, later.
