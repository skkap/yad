---
status: amended by 0040 — a grant may not name a variable that moves a run off its account (ANTHROPIC_API_KEY, CLAUDE_CONFIG_DIR, CODEX_HOME and the rest of protocol/v1's list) — and by 0062, where `[workdirs] path_sources = false` switches sources on the machine off, which no value of roots could
date: 2026-09-19
---

# The owner trusts the hubs it connects; YAD does not police what a hub sends

Until now YAD treated a hub as untrusted input. Grant names were filtered by
shape and a reserved list ([0024](0024-grants-are-named-as-secrets.md)), and a
`path` source was refused outside roots the owner had listed
([0033](0033-sources-reach-only-what-the-owner-allows.md)). The owner has set
that aside, for one reason that the filters cannot get around. A hub writes the
brief. A brief can tell the harness to read any file, send any secret anywhere,
or run anything the machine allows, and the harness auto-approves (0015).
Filtering environment variable names does not stop a hub that can simply ask.

So the trust boundary is the one [0015](0015-owner-environment-is-the-trust-boundary.md)
already names: the machine and its owner. A runner is not shared between people
who do not trust each other's hubs. It runs somewhere the hub's users are
allowed to run things, and any secret the machine holds is assumed reachable by
the hubs its owner connected.

What changes:

- **Grants.** Any valid environment variable name is accepted. A short deny list
  remains, and only for names that would break the run rather than attack it:
  `PATH`, `HOME`, and the loader variables `LD_*` and `DYLD_*`. The secret-shaped
  suffix rule and the namespace reservations of 0024 go. Grants are still
  delivered to the harness process alone, kept out of the prompt, logs and
  events, and deleted when the run ends. That protects the secret from being
  recorded, not the machine from the hub.
- **Path sources.** `[workdirs] roots` stays, but when it is unset it defaults to
  the owner's home directory, where 0033 refused every folder source.

What stays:

- **Owner configuration is never a protocol field.** Permission mode, sandbox,
  capacity, caps and accounts are set on the machine. This is about who decides
  the machine's settings, not about distrust of hubs.
- **A grant may not move a run off its account**
  ([0040](0040-a-grant-may-not-move-a-run-off-its-account.md)). The variables
  that choose a harness's credential or home — `ANTHROPIC_API_KEY`,
  `CLAUDE_CONFIG_DIR`, `CODEX_HOME` and the rest of the list in
  `protocol/v1/grant.go` — are refused as grant names, so that an account's
  limits, failover, health and event labels stay true.
- **Hub input is data to YAD itself.** YAD never executes a hub string, never
  passes one to a shell, and still refuses git argument injection (a leading
  `-`, remote helpers). Those guards prevent bugs, not attacks.
- **Tokens are never logged, printed or transmitted** beyond where they are
  delivered.

## Considered options

**Keep the filters.** They cost owners legitimate grants (`DATABASE_URL`, cloud
deploy keys) and folder sources, while the brief itself leaves the machine open
to any hub that wants it. **Remove the deny list too.** Nothing security-related
depends on it. It stays only so that a hub mistake cannot unset `PATH` and make
every run fail in a way nobody can trace.
