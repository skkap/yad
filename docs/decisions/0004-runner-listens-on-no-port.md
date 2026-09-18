---
date: 2026-09-18
---

# The runner opens no listening port

Runners sit behind NAT, on café networks and in private subnets nothing can
reach. The runner makes outbound HTTPS requests and nothing else; its only
socket is a Unix socket for its own CLI. That also removes authentication of
inbound connections entirely, which is the part of a daemon most often done
badly.

Monitoring follows from it: health travels in every sync, run metrics in every
result, and locally `yad status` reads the control socket. There is no metrics
endpoint.

## Considered options

**A local HTTP server for health and shutdown**, as Multica runs on
`127.0.0.1:19514`. Convenient, and a port on every machine. **A Prometheus
endpoint**, as Anthropic's self-hosted runner exposes on `:8080`. Standard
dashboards, and it reverses this record. Revisit only with a new decision record and
a reason, not a preference.
