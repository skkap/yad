---
date: 2026-09-18
---

# The hub closes sessions; the runner reclaims their workdirs

A session's workdir stays warm between runs, because resuming wants its
checkout. The hub closes a session explicitly when its work is done, and the
runner deletes the workdir. An idle session also expires after an owner-set TTL
(default 14 days), reported to the hub as expired; under disk pressure the
oldest idle sessions go first. A run naming an expired session fails with that
reason, and the hub decides whether to start fresh.

## Considered options

**TTL only** — one verb fewer, and finished work keeps its checkout for two
weeks. **Delete after every run** — nothing warm, which contradicts resumable
sessions.
