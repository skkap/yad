---
date: 2026-09-18
---

# Long-lived credentials stay on the machine; hubs grant per run

`gh`, SSH keys, docker and harness logins are the runner owner's, set up on the
machine and advertised as host tools and accounts. A hub may attach **grants** to
a run — short-lived secrets scoped to that run, such as a Zumino token limited to
one task — delivered in the environment or a `0600` file, never in argv, and
destroyed when the run ends. The harness never sees the runner's own credential,
so what it writes is attributable and scoped (Multica's per-task token).

## Considered options

**Hubs deliver everything**, GitHub tokens included — setup-free machines, and
every hub holding your long-lived credentials. **Nothing secret in the protocol**
— then the harness can only reach Zumino as the machine's owner.
