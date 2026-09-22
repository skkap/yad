---
date: 2026-09-22
---

# An override that names nothing lets PATH decide

Every harness and host tool has a path override, `YAD_<ID>_PATH`, for a daemon
started without its owner's shell `PATH`. `internal/harness` and
`internal/hostool` read it in two different ways. A harness whose override named
a file that was not there was reported `present: true` with a start failure.
The same override on a host tool made it `present: false` with an error. A hub
reads both from one capability document and cannot know that the rule depends
on the kind (DEV-68). The two packages had drifted apart in their messages too.

## Decision

One rule, in one place (`internal/probe`), which both packages call:

- **An override names nothing** when nothing is at the path, or what is there
  is a directory.
- **When it names nothing, PATH decides.** If PATH has the binary, it is
  `present`, it is the binary runs start (`harness.Locate` goes through the
  same lookup), and the override is reported in `warnings`, not in `error`.
  So a harness stays drivable. If PATH has none, it is `present: false`, with
  an `error` that names the override and says to fix or unset it.
- **A file that is there but is not executable, or will not start**, is
  `present: true` with an error: installed but broken.

So `present` means **the runner found a binary**, under the same rule for
harnesses and host tools. For a harness, that binary is the one every run
starts. For a host tool, it is the binary that was probed. When this was
decided, yad's own git (`internal/workdir`) and a harness's children still
looked host tools up on PATH, a separate gap (DEV-107) that
[0045](0045-runs-use-the-host-tools-detection-resolved.md) closed: a run now
uses the host tool detection found, through the same lookup. `present`
says nothing about whether the binary works: `error` says that.

`HostTool` gained `warnings` to carry the fallback. The field is additive, and
it follows the same rule as `HarnessReport.warnings`: the runner's words, never
a path or anything a child printed.

## Rejected

- **Absent, with PATH not consulted.** This was host tools' old rule. Its
  argument was that falling back would hide a mistyped override behind "no
  docker here". The warning answers that argument without its cost. The cost
  was an owner who uninstalled one copy of a harness and left the override
  pointing at it. Their machine would stop taking runs from every hub, even
  though a working copy was on PATH.
- **Present and broken.** This was harnesses' old rule. It made `present`
  claim a binary that is not there.
