---
date: 2026-09-18
---

# Run events are normalised and streamed live

Adapters translate each harness's stream into one event set — text, thinking,
tool call, tool result, status, usage, error — numbered per run, spooled in
SQLite and uploaded in batches of about a second, surviving a network drop. The
raw harness stream is kept locally for diagnosis and never sent. A hub never
parses a Claude or Codex format, so a harness format change is YAD's release,
not every hub's.

## Considered options

**Raw passthrough** — zero translation, and every hub parses every harness
forever. **Result only** — the smallest protocol, and no live view of a run that
takes hours.
