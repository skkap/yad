---
date: 2026-09-29
---

# The owner's own account variables are removed from every run

Answers the question [0040](0040-a-grant-may-not-move-a-run-off-its-account.md)
left open: whether the owner may put a run on a credential of their own by
setting it in the runner's environment. DEV-62.

0040 stopped a **hub** from moving a run off its account: a grant may not name
a variable that chooses whose credential a harness uses. The **owner's**
environment could still do it. `supervise.Scrub` removed `ANTHROPIC_API_KEY`
and every `CLAUDE_CODE_*` from each child and let the rest of 0040's list
through — `ANTHROPIC_PROFILE`, `ANTHROPIC_CONFIG_DIR`, the federation pair,
`ANTHROPIC_BASE_URL`, `OPENAI_API_KEY`, `OPENAI_BASE_URL` and others (DEV-73).
Any of them set on the machine ranks above the login in the account's home,
so the account layer told the same lie 0040 was written against: events naming
an account the tokens did not come from, limits that never fire, failover with
nothing to fail over, health calling the account free for ever.

DEV-26 found the same failure by another door. Its login check ran
`claude auth status` with an unscrubbed environment while the run's child was
scrubbed, so the check could answer "logged in" on an `ANTHROPIC_API_KEY` the
run never saw, and leave a useless account marked free.

## Decision

The owner decided (2026-09-29) that `Scrub` removes the list and `yad doctor`
warns. The rest was settled while building it, in DEV-62, and is recorded on
the ticket.

- **`Scrub` removes every variable 0040's list names**, from the list itself.
  `protocol/v1` exports `AccountVariable(name)`, the predicate
  `Grant.Validate` refuses by, and `Scrub` calls it. There is no second copy
  to drift. It applies to every child `supervise.Start` runs, not only a
  harness: a setup hook or a git hook that started a harness would pass the
  variable to it.
- **The harness home variables pass.** `CLAUDE_CONFIG_DIR` and `CODEX_HOME`
  are on the list because a grant naming one would move a run to a home a hub
  chose. The owner's own copy is different. With no accounts configured it is
  the harness's own login: the one the default login check
  ([0053](0053-a-harness-on-its-own-login-is-checked-like-an-account.md)) asks
  about, and the one `codex.DefaultHome` reads, from the same environment.
  With an account, `account.Env` appends the account's home after the scrub
  and wins, since `os/exec` keeps the last of a repeated name. Removing it
  would move every no-account run off the login the owner configured.
  `internal/account`'s tests hold the exception to the account homes.
- **What the account layer sets still arrives.** Only the inherited
  environment is scrubbed. An account's home and a token account's
  `CLAUDE_CODE_OAUTH_TOKEN` ([0054](0054-a-claude-account-may-be-a-token-and-every-account-shares-the-machines-config.md))
  are appended after the scrub, and so are a run's grants.
- **`yad doctor` warns about each one set.** It gives the name, never the
  value, says what the variable does and that it is removed from every
  harness yad starts, and names the two ways to get what the owner wanted:
  an account logged in with that credential, or a grant under another name
  for a project's own use. It is a warning and changes no exit code. Doctor
  sees the shell it runs in, and a service manager starts the daemon with an
  environment of its own, so the daemon logs the same warning about its own
  environment when it starts.
- **The probe environment is the run environment.** Any code that asks a
  harness about an account asks it in the environment the run will get. That
  is why the login check goes through `supervise.Start`. The rule is written
  on `supervise.Spec.KeepEnv`, the one field that could break it.
- **`KeepEnv` stays unwired.** It is the field an owner's "keep my
  `ANTHROPIC_API_KEY`" setting would feed. Whoever wires it to configuration
  refuses, or warns in `yad doctor` about, a kept account variable on a
  harness that has accounts, and keeps it for that harness's login check as
  well as its runs. A test fails as soon as anything outside `supervise` sets
  it, and names the rules.

API-key billing, then, is an account like any other. Claude logged in to a
Console account inside an account's home, or Codex logged in with
`--with-api-key`, is a login yad can see, check and report truthfully.

## Considered options

**Keep `KeepEnv` and let the owner opt a key back in.** The ticket's first
framing. That is the configuration in which the account layer lies. Nothing
anyone reads (events, limits, health) would say which credential a turn really
spent, and an owner who wants API billing has a truthful route through an
account.

**Warn only, and leave the variables in place.** The account layer would then
be true only on machines where nobody had exported one of these, and the
owner's shell profile decides that. `yad doctor` is where an owner hears about
it; the runner still has to behave the same way whatever it finds.

**Move the list into an internal package that both `protocol/v1` and
`supervise` import.** `protocol/v1` imports nothing of ours (ARCHITECTURE.md
§1), because Go hubs import it. A small exported predicate costs less than
bending that rule, and it tells a Go hub the same thing `Validate` does. The
signature is additive and stays; the list behind it may grow.

**Remove the home variables too, for symmetry with the grant list.** That
would silently move a no-account runner whose owner set `CODEX_HOME` onto
`~/.codex`, while `codex.DefaultHome` and every command yad prints named the
owner's home (DEV-67).
