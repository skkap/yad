---
date: 2026-09-29
---

# Only the latest recorded harness protocol is pinned

[0037](0037-a-codex-run-is-its-own-turn-and-its-protocol-is-pinned.md) pins
the Codex app-server protocol "per recorded version", and two were pinned side
by side: 0.147.0, which the adapter was built against, and 0.157.1, recorded
when forks arrived (DEV-128, DEV-48). Keeping the older one had a price the
moment the newer one was worth using. Codex 0.157.1 answers every
`thread/resume` with a deprecation notice asking for `excludeTurns: true`;
0.147.0 refuses that field (`thread/resume.excludeTurns requires
experimentalApi capability`, measured for DEV-139). Using it would have meant a
branch per version in the adapter, or asking 0.147.0 for its experimental
surface — for a release no machine yad knows of still runs.

## Decision

The owner's call (DEV-153): yad supports the latest harness release it has
recorded, not the ones before it.

- **One pin per harness.** `internal/adapter/codex/schema.go` holds one
  version and its surface hash, `pinnedVersion` and `pinnedSum`, not a map.
- **Pinning a new release drops the older in the same change**: its hash, its
  schema bundle, its recordings, its hand-written fixtures and every doc that
  names it as the one to install. `TestPinnedSchema` fails while a second
  `testdata/codex-*` directory is there, so the step cannot be forgotten.
- **An older codex is warned about, not refused.** Its surface hash differs
  from the pin, so `yad doctor` and the capability document carry 0037's
  drift warning, whose next action now names the one release to install.
  It is still driven: the warning says what was verified, and a run that uses
  something the older release lacks fails with Codex's own refusal. Codex
  0.147.0 on this change: `this codex's app-server protocol differs from the
  one this yad was built against (codex 0.157.1) …`.
- **A request may use what the pinned release takes**, with no branch for a
  release before it. That is what lets `thread/resume` send `excludeTurns`
  (DEV-139).
- **Measurements stay dated.** Decision records and comments that say what
  0.147.0 did (0013, 0049, 0050, `codex.go`, `login.go`) keep saying so: they
  are what was measured on which release, not a claim that it is supported.
  One re-measured on the pinned release replaces the old one where it lives.

Claude has no pin for this rule to cover today. Nothing compares an installed
Claude's version, or its stream format, with a recorded one: the flags probe
(`claude.FlagsCheck`) asks the installed `claude --help` for every flag a run
passes and refuses one that lacks any, whatever its version, and the one
version floor in the code — `list_models`, answered from 2.1.283 — is a
message naming the release that answers, not a pin. Its fixtures sit under
2.1.276, 2.1.278, 2.1.280 and 2.1.284 because each was recorded when it was
first needed; none is recorded twice, so none is an older copy of a newer
one. They are left where they are. Should Claude's protocol ever be pinned the
way Codex's is, this rule applies to it from that change.

## Considered options

**Keep every recorded release pinned.** What 0037 did. It keeps a runner on an
old Codex free of a warning, and costs a branch per version for every request
field a newer release adds or deprecates, a second set of recordings to keep
replaying, and a test matrix that grows with every weekly release pinned.
Nobody asked for it: the machines yad runs on install the pinned release
(`machines/guest/agent.sh`, `docs/containers.md`).

**Pin a window — the latest two.** Leaves a machine one release behind
unwarned, and still needs the per-version branch this record exists to avoid,
for as long as the older release in the window lacks what the newer one takes.

**Refuse an unpinned codex.** Rejected in 0037 and still wrong: most releases
change nothing the adapter reads, and a runner taken out of service for a
reworded field is worse than a warning.
