---
date: 2026-09-19
---

# A grant is named as the secret it is

A grant reaches the harness as an environment variable: `NAME=value`, or, for a
file grant, a `0600` file whose path is in `NAME`. An environment variable is a
lever. `LD_PRELOAD` loads code into every process, `ANTHROPIC_BASE_URL` sends
the owner's traffic to whoever the hub names, `HTTPS_PROXY` with
`NODE_EXTRA_CA_CERTS` does the same through a proxy, and `IS_SANDBOX` switches
off Claude's refusal to bypass permissions as root. A hub is untrusted input,
and a grant that moves any of these widens what the owner configured (0015).

So `protocol/v1` validates every grant's name, and a run carrying one that
fails is refused: by the hub before it is queued, and by the runner at the
claim, with the `refused` result of
[0019](0019-a-run-starts-once-its-claim-is-acknowledged.md). It is never run
with the grant stripped, since a run missing the secret it was given fails in
ways nobody can trace back. The rules, in `protocol/v1/grant.go` with a reason
beside each reserved entry:

1. an upper-case environment variable name, `[A-Z_][A-Z0-9_]*`, which is also a
   plain file name, never a path;
2. not reserved: `PATH`, `HOME`, `SHELL`, `TMPDIR`, `BASH_ENV`, `ENV`,
   `NODE_OPTIONS` and `IS_SANDBOX` by name; `LD_`, `DYLD_`, `YAD_`, `CLAUDE`,
   `ANTHROPIC_`, `CODEX_`, `OPENAI_`, `GIT_`, `NODE_`, `NPM_CONFIG_` and `BUN_`
   as namespaces, and `AWS_`, `GOOGLE_` and `AZURE_`, whose credential chains a
   harness pointed at Bedrock, Vertex or Azure reads — a grant there signs the
   owner's model traffic as the hub's account;
3. named as a secret: it ends in `_TOKEN`, `_KEY`, `_SECRET`, `_PASSWORD`,
   `_CREDENTIAL` or `_CREDENTIALS`.

The cloud namespaces also refuse a legitimate deploy credential for AWS,
Google or Azure; handing one over safely needs the owner's allowlist below.

The third rule is what lets the list in the second be incomplete. The variables
that steer a program — proxies, CA bundles, `SHELLOPTS` and `PS4`,
`JAVA_TOOL_OPTIONS`, and the next one a runtime invents — are not named like
secrets. The reserved list is for the secret-shaped names that steer anyway.

## Considered options

**A denylist alone.** It was the first version, and the first review found
`HTTPS_PROXY` plus `NODE_EXTRA_CA_CERTS` routing the harness's API traffic
through the hub; every list of that kind is one variable short. **An allowlist
in the owner's config**, naming each grant a hub may send. The strongest, and a
setting every owner must maintain before any hub can hand a run a token — the
grants story of epic E7 can add it on top. **Stripping a reserved grant and
running anyway.** Silent, and the run then fails for a reason nobody sees.
