---
date: 2026-10-16
status: accepted
---

# A runner's owner may give every run standing instructions

Some knowledge is the machine's, not any hub's: "Docker is the colima
context, leave colima-ci alone", "never pkill by name, CI shares this box",
"a browser is installed". It is true for every run on the machine whichever
hub sent it, and only whoever runs the machine knows it. JapanLaw #1
(ZUM-100) put it in a repository file, which is wrong on any other runner and
impossible for a repository the owner cannot edit. YAD-199.

## Decision

The owner decided on 2026-10-16 (ZUM-128): **it is yad's own configuration,
hub-neutral, and reaches every harness the runner starts.** Not a Zumino
setting, and not a protocol field a hub could set.

```toml
[instructions]
text = '''
Docker is the colima context; leave colima-ci alone.
'''
file = "INSTRUCTIONS.md"   # relative to this profile's config directory, or absolute
```

**Delivered through the context.** The runner appends them to the run's
`brief.context`, after the hub's, under a heading of its own — `## This
machine` — so the harness can tell the hub's words from the owner's. That
channel is the one 0050 made reach every run of a session and outlast a
compaction: Claude's system prompt, Codex's developer instructions,
OpenCode's instruction file. A run whose hub sent no context gets the
section alone. The instruction — the run's user turn — is never touched.

**When they are read.** `file` at every run's start, so an edit reaches the
next run with no restart; `text`, like every other key of `config.toml`, when
the daemon starts. The ticket's plan had both read at the run's start;
re-reading `config.toml` for one section would make a run's behaviour depend
on a file the rest of the daemon reads only at start, and a typo saved in it
would fail runs rather than the next start. The file is where text that
changes belongs. No `~` is expanded, as in `[env]`; a path starting with one
is refused at load with the fix.

**A cap, and a refusal rather than a cut.** 16 KiB for the text and the file
together, about four thousand tokens that every request of every turn pays
for. `instructions.text` over it is refused when `config.toml` loads. A file
over it, missing, a directory or not UTF-8 fails the run `prepare_failed`
before anything is made for it, with the reason: a rule cut in half can say
the opposite of the whole, and a run started without the rules would do what
they forbid with nothing saying why. `prepare_failed` rather than `refused`
because nothing is wrong with the run: the owner fixes the file and the same
run goes through.

**What a hub learns: that they exist.** The capability document carries
`instructions: true` when either key is set, and nothing when neither is —
never the text, its size or the file's path, which may name the owner's
arrangements and directories. An edit to them therefore moves no fingerprint.

**Where the owner sees them.** `yad doctor` says how much every run is given
and from where, read as the next run will read it, or prints the error that
refuses runs. `yad status` says the same from the daemon. Neither prints the
text: it is in the files they name. `yad config apply` carries the section
like any other and reports `instructions.text` by name, as it does `[env]`.

## Consequences

- One protocol field added within v1, `Capabilities.instructions`, sent only
  as true; a hub generated earlier ignores it.
- The heading is the runner's and fixed: a hub that wants its own words above
  the owner's already has `brief.context`.
- Per profile, like the rest of `config.toml`: two runners on one machine may
  give different instructions.
