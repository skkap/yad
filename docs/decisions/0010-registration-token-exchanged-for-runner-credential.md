---
date: 2026-09-18
---

# A one-time registration token is exchanged for a runner credential

A hub issues a short-lived registration token ("Add runner" in its UI);
`yad connect <hub> --token …` exchanges it for a per-runner credential that the
hub can revoke and rotate. Only the credential is stored, `0600`. It is the model
GitHub, Buildkite, GitLab and Anthropic's runners share, and it lets one machine
be revoked without touching a person's own token.

## Considered options

**Reuse a person's PAT**, as Zumino and Multica do — no new token type, but the
runner *is* the person: no per-machine revocation, and every write looks human.
**A runner keypair with signed requests** — no bearer secret after registration,
and signature checking every TypeScript hub has to get right.
