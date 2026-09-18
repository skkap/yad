---
date: 2026-09-18
---

# The owner's environment is the trust boundary; YAD adds no guardrails

A headless harness cannot ask anyone for permission, so runs auto-approve: the
permission mode and sandbox are the runner owner's configuration per harness, and
no hub can set or widen them. YAD does not build its own sandbox, policy engine or
tool allow-lists — too large a problem to solve well here, and the harnesses
already ship guardrails an owner can turn on (Codex's sandbox modes, Claude's
permission modes). What YAD ships instead is a recommendation: run a runner on a
dedicated machine, VM or container, never on a personal laptop with live
credentials, and one runner per trust domain.

## Considered options

**Named policies on the runner**, referenced by name from a run — the
recommendation during the interview, dropped as more machinery than v1 needs.
**Per-run permission fields, clamped by the runner** — the protocol would carry
bypass-shaped fields, and the clamp is where the security bug would live.
