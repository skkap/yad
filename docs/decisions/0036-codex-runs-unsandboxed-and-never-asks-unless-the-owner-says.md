---
date: 2026-09-19
---

# A Codex run never asks and has no sandbox, unless the owner says otherwise

Decision [0015](0015-owner-environment-is-the-trust-boundary.md) makes the
owner's machine the boundary and leaves each harness's own guardrails to the
owner. For Claude that became `bypassPermissions` when `config.toml` sets no
`permission_mode`. DEV-21 settles the same for Codex, whose guardrails are two
settings — an approval policy and a sandbox mode — sent with `thread/start`
and `thread/resume`.

**The defaults are `approval = "never"` and `sandbox = "danger-full-access"`.**
A run is unattended: a policy that asks would ask nobody. And a run needs what
the owner's machine offers — `git push`, `gh`, installing a dependency — which
Codex's own sandboxes take away: `workspace-write` cuts the network by
default, and `read-only` cuts everything a coding run is for. This is the same
place Claude's default lands, for the same reason.

**The owner's values win.** `[harness.codex] sandbox` and `approval` are sent
exactly as written, for a new thread and for every resumed one, so changing
them takes effect at the next run of every session. An owner who wants Codex's
sandbox sets `sandbox = "workspace-write"`. They are runner configuration, as
0015 requires: no protocol field carries them, and a hub cannot widen them.

**A request for approval that still arrives is declined.** With a policy
other than `never`, Codex asks before a command or a file change it would not
run on its own. Nobody is there, and the owner who chose that policy chose
for Codex to stay inside it, so the adapter answers every such request no —
commands and file changes `decline`, a request for more permissions with
nothing granted, an MCP server's question `decline` — and says so in the run's
events (`approval declined: …`). A request no answer fits, such as a question
for the user, is refused as a JSON-RPC error. Every thread's requests are
answered, not only the run's: a subagent waiting on an approval would hang the
turn. This is what the Claude adapter does with a `can_use_tool` prompt
([0021](0021-claude-runs-end-at-the-last-result.md)).

## Considered options

**`workspace-write` as the default sandbox**, with `never` — proposed during
the task, as the safer-looking middle. Rejected: it cuts the network, so the
runs a hub sends most — push a branch, open a PR, install dependencies — fail
on a stock runner, and it adds a guardrail of YAD's own choosing, which 0015
says YAD does not do. An owner who wants it sets one line.

**Accept every approval request**, as Multica does. It makes the owner's
policy mean nothing: a sandbox that the model can ask to leave, and is always
let out of, is not a sandbox. Declining keeps `untrusted` and `on-request`
meaningful for the owner who picks them.

**Validate the two settings against Codex's enums.** A misspelt value reaches
Codex, which refuses the thread, and the run fails with Codex's own message and
a pointer to `config.toml`. Validating here would pin YAD to one Codex
release's list of modes.
