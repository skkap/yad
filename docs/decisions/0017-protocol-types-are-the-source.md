---
date: 2026-09-18
---

# The Go protocol types are the source; OpenAPI is generated from them

`protocol/v1` holds the wire types. `yad hub`'s operations are declared against
them, and `protocol/v1/openapi.yaml` is generated from those operations and
committed. A test fails when the committed file differs from what the code
generates, so every contract change shows up in review as a spec diff; a
breaking-change check runs against the last released spec. TypeScript hubs
generate their types from the committed file, and `yad conformance <hub>` checks
any hub against it.

## Considered options

**A hand-written spec** — immune to accidental change by refactor, and a second
copy of every type to keep in step. The drift test gives the same protection.
**Protobuf and ConnectRPC** — typed both sides, and not plain JSON a person can
curl through any proxy.
