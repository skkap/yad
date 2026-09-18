---
date: 2026-09-18
---

# Multica is read for shapes, never copied

Multica's licence is Apache-2.0 plus conditions that win conflicts: no hosted
service for third parties and no embedding in a commercially distributed product
without a commercial licence (1a), and "built on Multica" in the user-facing
documentation of anything derived from its daemon or CLI (1c). Zumino customers
will run YAD, so any lifted line would bind YAD to both. Protocols, constants and
hard-won lessons carry no such obligation: read it, credit it in the commit, and
write every line fresh. Read the latest upstream, not a local copy.

## Considered options

**Lift with full compliance** — ship Multica's LICENSE and NOTICE, carry the
statement, and hold a commercial licence before any customer runs it.

## Consequences

[0001](0001-go-not-rust.md) chose Go partly to lift adapter code with
attribution. That reason is gone; its others stand.
