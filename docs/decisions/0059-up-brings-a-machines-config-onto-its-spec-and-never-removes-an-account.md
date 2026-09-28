---
date: 2026-09-29
---

# `yad-machine up` brings a machine's config.toml onto its spec, and never removes an account

[0052](0052-a-work-machine-is-a-lima-vm-built-from-a-spec.md) seeded a work
machine's `config.toml` from its spec on the first `up` only: afterwards
`yad connect` and `yad account add` wrote the machine's copy, and replacing
it would have disconnected every hub. So a spec's change never reached a
built machine — `tl-general` needed `accounts = ["tl"]` added by hand on
2026-09-26 — and `up` printed a diff for someone to act on.

Now `up` merges, on every run, through **`yad config apply <file>`**:

- **The spec owns every setting but the connections.** Name, labels,
  capacity, each `[harness.*]` section, `[sessions]`, `[supervise]`,
  `[drain]`, `[workdirs]` — what the spec leaves out goes back to the
  default, as it would in a fresh `config.toml`. A harness the spec has no
  section for keeps only its accounts. The ticket named the first four; the
  rest are included because a spec's `[drain] wait` that never reached the
  machine is the same bug.
- **Connections are the machine's.** Each needs a credential only
  `yad connect` on the machine makes, so the machine's are kept and a spec's
  `[[connection]]` is not taken — `apply` says so, with the command that
  makes one.
- **A spec's accounts are made sure of, never subtracted.** Its labels come
  first, in its order (the order breaks ties — 0039), then every label only
  the machine lists, in the machine's order. A label the spec does not list
  may be one added at the machine or by a hub (0057), and a diff of two lists
  cannot tell that from one the spec dropped — the same reason 0043 gives for
  never deleting a home because a label disappeared. Taking an account off a
  machine stays `yad account remove`'s, the one path that knows whether a run
  is on it.

**yad does the merge, not the kit.** Merging TOML in shell would be sed over a
format with arrays and tables; `apply` loads both files with the config
package, so the machine's copy stays exactly as yad writes it, and it writes
under the `config.toml.lock` every other writer takes (0057), so an account a
hub adds during an `up` is neither lost nor overwritten. It writes only when a
setting differs — compared as the file would hold them, so `labels = []` and
no labels are the same — and prints each setting it changed.

**The runner restarts only when it is owed one.** The daemon reads
`config.toml` once, at start, but for the account lists (0043), and a restart
drains every run it holds, so `up` no longer runs `yad service install` every
time. It does when `config.toml` changed, when root's step installed a yad
that differs from the one there (it now installs only then), or when the
service is not enabled and running — and a restart owed by an `up` that
stopped short of making it is kept on disk and made by the next. A harness
upgrade is not a reason: the runner probes its harnesses at its own pace.

**A yad without `config apply`** — a pinned `YAD_VERSION`, or the latest
release before this shipped — gets 0052's behaviour: seeded once, a diff
printed, and a note naming a newer yad.

## Considered options

**Replace the machine's file with the spec's and re-add the connections.**
Simpler, and wrong twice: an account logged in at the machine or by a hub
would be dropped from the list, and a hand edit made on the machine would
be lost silently rather than reported as a change.

**Accounts mirror the spec.** Removing what the spec does not list would make
the spec the whole truth, but it would delete accounts a hub added — with
their logins — whenever a spec was applied, and a removal needs the daemon to
know whether a run is on the account, which a file edit cannot tell it.

**The merge in the kit, in shell.** No new command, but TOML arrays and
tables in sed or awk, a second writer that takes no lock, and a file that no
longer looks like the one yad writes.
