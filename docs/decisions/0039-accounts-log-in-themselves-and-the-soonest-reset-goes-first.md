---
date: 2026-09-19
---

# Accounts log in through the harness, and the one that resets soonest goes first

Settles the parts of [0013](0013-accounts-fail-over-and-limited-runs-wait.md)
that epic E6 needs before it is built.

- **Login is the harness's own.** `yad account add claude work` runs the harness
  login inside that account's home (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`), with the
  owner present. YAD stores no tokens of its own for accounts. An account whose
  login has expired is marked **needs login**: it is skipped like a limited
  account, and the state is reported to every hub in health. A way to finish a
  login remotely, and a notification to the owner, are later work.
- **Order is by reset, not by list.** A new run, and a run moving off a limited
  account, takes the free account whose usage window resets soonest. Quota that
  is about to be refreshed is used first, so nothing is wasted. The owner's list
  breaks ties and names which accounts take part at all.
- **Limits are reported.** Every sync carries each account's state and, where
  the harness says, its window use and reset time, so a hub can see why a
  runner is not claiming.
- **A session that cannot move stays put.** If the DEV-24 spike shows a
  transcript cannot be shared between account homes, a session is pinned to its
  account and its run waits for that account's reset.
- **Using several subscriptions is the owner's decision.** YAD does not second-
  guess it. The terms-of-service question in 0013's consequences is withdrawn.

## Considered options

**Owner order**, as 0013 first said: predictable, and it lets quota on a later
account expire unused. **Most headroom first**: spreads load, but ignores that a
window about to reset is free quota. **A long-lived token per account**: suits a
headless machine, and is not relied on until the harnesses commit to one.
