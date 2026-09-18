---
date: 2026-09-18
---

# A hub is a role, and a runner connects to many of them

YAD has one runner protocol. Anything that hosts its server half is a **hub** —
Zumino by embedding it so its users can bring their own runners, yashiki by
embedding it or by using `yad hub`, which is the same binary in server mode. A
runner holds one connection per hub and shares its capacity across all of them.
The operator wanted no mandatory central service, runners "anywhere I like", and
a Zumino customer able to attach their own machine running on their own
subscriptions; this is the only shape that gives all three.

`yad hub` in v1 is headless: the protocol server, a SQLite store, a small API
for services to submit runs and read events, and `yad submit` / `yad watch` for a
person. No web UI. It is also the reference the conformance suite is checked
against and the fake every test runs against.

## Considered options

**Every service hosts it, no `yad hub`.** Same runner; less to build here, more in
every service, and tests would still need a fake server — which is `yad hub`.

**One personal hub, services are its clients.** One fleet view and scheduling in
one place, but it is the separate service the operator did not want, and bringing
your own runner to Zumino would need a hub per customer.

**A Zumino-only client-side driver** — YAD polls `GET /queue` with a PAT and
claims by PATCHing status. Fits Zumino's current rules and nothing else; it was
the recommendation until the operator called Zumino's side temporary.

## Consequences

Supersedes "one control plane per runner" and the per-control-plane **driver**
from [0002](0002-generic-core-zumino-first.md): every hub speaks the same
protocol, so there is nothing to translate. The protocol must assume a hostile
hub from day one — a customer's runner is somebody else's machine.
