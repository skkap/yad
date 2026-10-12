---
date: 2026-10-12
---

# A grant may not rename the login

Amends [0038](0038-the-owner-trusts-the-hubs-it-connects.md)'s grant list, for
the reason [0040](0040-a-grant-may-not-move-a-run-off-its-account.md) gives
for its own. DEV-193, found by review on DEV-188.

Claude on macOS keeps a home's login in the Keychain, in an item whose account
attribute is `$USER` ([0070](0070-a-claude-accounts-keychain-login-goes-with-its-home.md)),
and yad finds and removes that item under the runner's own `USER`
(`account.keychainAccount`), which `supervise.Scrub` leaves alone. A grant
named `USER` passed `Grant.Validate`, which refused only `PATH`, `HOME`,
`LD_*`, `DYLD_*` and the account list. `Exec.grants` delivers it after the
scrub, `account.TurnEnv` does not replace it, and the Claude adapter keeps it.
A run carrying `USER=builder` therefore had Claude look for the account's login
under `builder`, and failed to authenticate although the account is logged in
— a run moved off its login by a grant, the failure 0040 refuses grants for.
The owner's `[env]` already refuses `USER` for the same item
([0081](0081-the-owner-declares-an-environment-for-everything-yad-starts.md));
a hub's grant was the way left open.

## Decision

The owner's (2026-10-12): **a grant may not be named `USER` or `LOGNAME`.** A
run carrying one is refused whole — by `yad hub` before it is queued, by a
runner at the claim with the `refused` result, before any workdir is built —
and the refusal names the variable, why, and the way round it: send the value
under another name and have the brief say which. The rest was settled in
building it.

- **On the deny list, not the account list.** `deniedGrantNames` in
  `protocol/v1/grant.go`, beside `PATH` and `HOME`: names that would break the
  run rather than attack it. `accountGrantNames` is also what `supervise.Scrub`
  removes from the owner's own environment (0060, through
  `v1.AccountVariable`), so a login name there would strip the owner's `USER`
  from every child and move the very Keychain name this protects.
- **Both spellings.** `LOGNAME` is the same login name by POSIX's name, and
  which of the two a tool reads is the tool's choice — Claude reads `USER`
  today. Refusing the pair means a harness that reads the other one tomorrow is
  refused before anyone thinks to add it, as 0040 refuses the
  `CLAUDE_CODE_USE_*` family whole.
- **Matched in any case**, as the rest of the deny list is and for its reason:
  the list catches a hub author's mistake, and the over-refusal (`user`) costs
  a rename.
- **Every harness.** A Codex run is refused `USER` too: a hub checks the list
  without knowing which adapter reads what, as 0040 says of its own.
- **The owner's `[env]` is unchanged.** It keeps its `USER` refusal, which
  exists because `yad account remove` reads `USER` with no daemon and no
  layer, and it does not gain `LOGNAME`: yad reads `LOGNAME` nowhere, and an
  owner's login name set for everything yad starts puts no two readers of it
  at odds. A grant's `LOGNAME` would reach the harness alone, beside the
  owner's `USER`, which is the disagreement refused here.
- **Documented where the rest is:** the `Grant.name` description in both
  OpenAPI documents (from the doc tag in `protocol/v1/run.go`), HUB.md's
  validation list, ARCHITECTURE.md, docs/run-it-safely.md and DOMAIN.md. A
  test already fails when HUB.md or the doc tag leaves out a name the deny list
  refuses. `make check-breaking` (oasdiff) reports no breaking change: the
  rule is a description and a validation, not a type.

## Considered options

**Give Claude the owner's `USER` whatever the grant says.** The adapter would
set `USER` back after the grants. The hub would believe it delivered a variable
that never arrived, which is the silent strip 0024 and 0038 refuse; and every
other tool the run starts would still see the hub's.

**Put them on the account list.** Refused by the same check, but read by
`Scrub` too, which would remove the owner's own `USER` from every child — see
above.

**Leave it in the backlog.** No hub sends `USER` today that anyone knows of,
but nothing stops one, and the failure it causes reads as a logged-out account.
