---
date: 2026-09-29
---

# An owner may switch sources on the machine off, with `path_sources = false`

[0038](0038-the-owner-trusts-the-hubs-it-connects.md) made `[workdirs] roots`
default to the owner's home directory where
[0033](0033-sources-reach-only-what-the-owner-allows.md) had refused every
source on the machine. That left no way back to 0033's default. `roots = []`
reads as "none listed", which is the home directory, and could not mean
anything else: the field is `omitempty`, so an empty list is dropped the next
time `yad connect`, `yad account add` or a hub's added account rewrites
`config.toml`. Roots could be widened or narrowed, never switched off (DEV-72,
raised in DEV-30's review).

The owner chose, on 2026-09-29, **an explicit toggle**: `path_sources = false`
under `[workdirs]`.

- **What it refuses.** Every source that reaches the machine rather than the
  network: a `path` source, and a `git` source whose URL is an absolute path
  or `file://` — the same set the roots govern, since a clone of a directory
  is a read of it (0033). The run fails `source_refused`, and the message names
  `path_sources = false` and `config.toml`, not the roots: an owner told a
  path was outside the roots would widen them and change nothing. A git source
  over https or ssh is unaffected.
- **Roots are kept, and ignored while it is off.** Setting both is not an
  error: switching back on should restore what the owner had.
- **Unset is today's behaviour.** Absent or `true`, sources on the machine are
  taken inside the roots, which default to the home directory — resolved at
  run time and never written into `config.toml`.
- **It survives every writer.** A `*bool` with `omitempty`, like a
  connection's `manage_accounts` (0057): absent stays absent, `false` is
  written back as `false` by `yad connect`, `yad account add` and `remove`,
  and the daemon's account writes. `yad config apply` takes `[workdirs]` from
  the spec whole ([0059](0059-up-brings-a-machines-config-onto-its-spec-and-never-removes-an-account.md)),
  this key with it. Read at start, like the rest of `[workdirs]`.
- **The capability document says so.** A runner with it off sends
  `path_sources: false`; one that takes them sends nothing, so its document —
  and every older runner's — is unchanged, fingerprint included. A hub offers
  a run opening a session with a source on the machine only to a runner that
  does not say `false`, and `yad hub` does. A run in a session already bound to
  such a runner is offered anyway: it can go nowhere else, and the runner's
  refusal names the setting, where a run left queued would say nothing. The
  field is additive; a hub that ignores it gets the refusal.

## Considered options

**Leave it.** Defensible under 0038 — a hub that writes the brief can have the
harness read any file, so refusing sources specifically buys little. But "the
owner cannot turn this off" was a property of a security-relevant setting
produced by a struct tag, not a choice anyone made.

**Drop `omitempty` and tell nil from empty.** `roots = []` would then mean
nothing, but only by writing `roots = []` into every `config.toml` that did not
list roots, where it reads as a setting the owner made. Keeping the home
default out of the file (`TestTheHomeDefaultIsNeverSaved`) is what rules it
out.

**A protocol feature instead of a field.** Features are what a runner does
beyond the v1 baseline; taking sources on the machine is the baseline, so an
older runner would advertise nothing and read as switched off.
