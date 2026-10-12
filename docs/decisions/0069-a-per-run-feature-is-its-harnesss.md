---
status: amended on 2026-09-30 (YAD-168) — the runner-wide strings are computed over the harnesses the machine can drive, not the build's catalog, and a machine that can drive none lists no per-run feature — and on 2026-10-12 (YAD-169), where a harness with a run held counts too, drivable or not
date: 2026-09-30
---

# A per-run feature is its harness's

Four protocol features are about what a run may do rather than what the
runner does: `steer`, `interrupt`, `effort` and `fork`. Until now each was one
runner-wide string, advertised only while every first-class adapter
supported it (decisions 0049, 0065), with a test holding the two together.
That was fine while Claude and Codex were the only harnesses and both did all
four. The next harnesses are driven over the Agent Client Protocol (YAD-44),
and ACP has no steer at all; Copilot, Cursor and Gemini have no fork. Made
first-class under the old rule, any one of them would switch the feature off
for Claude and Codex runs on every runner of that build. The owner decided on
2026-09-30 that the features become per harness first. YAD-158.

## Decision

**Each harness report carries `features`**: the per-run features a run on
that harness may use, from its adapter — `steer` when its turns take a steer
(`adapter.Steerer`), `interrupt` always (a harness is first-class only once
its turn can be ended without ending its session), `effort` when it is an
`EffortApplier`, `fork` when it is a `Forker`. Reported for first-class
harnesses only; a recognised one takes no run.

**A new runner-wide feature, `harness_features`, says the lists are there.**
With it, a harness's list is the whole answer for runs on that harness, and an
absent list means none. Without it — a runner older than the lists — the
runner-wide strings answer for every harness, as before. The feature string
is what tells the two apart: `features` is `omitempty`, like every list in the
document, so an empty list and a runner that never sent one look the same on
the wire.

**The runner-wide strings stay, meaning what they meant.** `steer`,
`interrupt`, `effort` and `fork` are listed runner-wide only while every
first-class harness's adapter supports them — computed from the same adapters
that fill the lists, so the two cannot disagree. A hub that never reads the
lists is therefore never told a run may use what its harness cannot; it is
only more cautious than it needs to be, holding back a Claude run's steer
because another harness this build drives takes none. "Every first-class
harness" is the build's catalog, not what is installed on the machine: the
strings stay a fact about the build, as they were, rather than moving with
detection. *(Amended below, YAD-168: every harness the machine can drive.)*

**Gating reads the run's harness.** `capability.RunMayUse(doc, harness,
feature)` is the one rule: the harness's list beside `harness_features`, the
runner-wide string otherwise, and the runner-wide string alone for any feature
that is not per-run (`start_at`, `drain`, `login` and the rest are the
runner's). `yad hub` uses it to offer an effort or a fork, to take a steer or
an interrupt at its service API, and to deliver one in a sync. A steer or
interrupt refused because the harness's list lacks it answers with the
alternative (put the text in a new run's brief; cancel instead) and not
"upgrade yad", which would not help.

**The adapters live in one list.** `capability.Adapters()` is what `yad
daemon` registers and what the lists are computed from, so what a harness
advertises and the adapter that drives its runs cannot come apart. The
runner's own refusals stay as they were: an effort or a fork handed to an
adapter that cannot is refused, class `refused`, and a steer to a turn that
takes none is an `error` event, class `steer_failed`.

## Amendment: over what the machine can drive (YAD-168)

OpenCode became first-class on the same day (YAD-44,
[0073](0073-opencode-is-first-class-through-a-generic-acp-core.md)), and ACP
v1 has no steer. Under the build's catalog every yad runner stopped
advertising `steer` runner-wide — including the many with no OpenCode
installed — and a hub reading only the runner-wide strings, Zumino's today,
stopped steering any run. The owner decided on 2026-09-30 that the strings
follow the machine.

**The runner-wide strings are computed over the harnesses this machine can
drive**: `capability.Features(reports)` lists a per-run feature when there is
at least one report that is `Drivable` — first-class, present, no `error` —
and every such report's `features` has it. A harness in the catalog and not
present, one present whose probe failed or whose default login is missing,
and a recognised one take nothing away. They are read from the
reports in the same document, so the two cannot disagree. A runner without
OpenCode advertises `steer`, `interrupt`, `effort` and `fork` runner-wide
again; one with OpenCode installed and working advertises `interrupt`,
`effort` and `fork`. Each harness's list is unchanged.

**A machine that can drive nothing lists no per-run feature.** It takes no
run, so a per-run feature has nothing to apply to; saying all four because
no harness objects would be true of nothing. It also keeps the strings moving
the safe way for a hub that reads only them: a feature appears when the first
harness is installed, rather than being advertised on an empty machine and
vanishing when that harness turns out to lack it.

**The fingerprint carries the change.** The strings and the reports are in
one document, so installing or removing a harness — or one's probe starting
or stopping to fail — moves the reports and the fingerprint, and the strings
with them whenever it changes what every drivable harness shares; a hub
re-reads the document when the fingerprint moves (HUB.md §3, the sync)
and never holds strings from one detection beside reports from another. The
price, weighed and rejected below before this amendment, is that a hub
reading only the strings sees a runner lose `steer` when OpenCode is
installed on it. That is the truth about the runs it may then be sent — one
may target OpenCode — and the cost of the other rule, every runner losing
`steer` whether or not it has OpenCode, was a hub that steers nothing.

One gap is accepted. The strings describe the harnesses drivable now, so a
harness that stops being drivable while a run on it is live — OpenCode's
binary gone mid-upgrade — no longer counts, and a hub reading only the
strings may steer that run. The runner answers with an `error` event, class
`steer_failed`, and the run goes on. Counting the harnesses with live runs
as well would close it, at the price of the document depending on the
runner's run state; one failed steer in that window was judged not worth it.
*(Closed by the second amendment below, YAD-169.)*

The conformance suite's document is unchanged, and still a probe: a yad
runner's runner-wide strings still only name what every harness it can drive
lists, with OpenCode or without it, which a test in `internal/conformance`
checks against both machines.

## Amendment: and the harnesses with runs held (YAD-169)

The gap the first amendment accepted is closed. **A harness counts in the
runner-wide strings while a run on it is held** — claimed, preparing,
running or waiting — whether or not it is drivable now:
`capability.Features(reports, held...)` lists a per-run feature when at least
one harness counts and every one that does has it. A drivable harness's list
is read from its report, as before; a held one that is not drivable is its
adapter's (`capability.HarnessFeatures`), since that adapter is what a control
sent to its run reaches. So with OpenCode's binary gone mid-upgrade and its
run going on, a runner with Claude beside it still leaves `steer` out of the
runner-wide strings until that run is over, and a hub reading only the
strings never steers it.

**Only the strings move.** Each harness report's `features` stays what its
adapter does, run or no run: a report says what the machine probed, and a hub
reading the lists already gates on the run's own harness. Nothing on the
wire is new — "held" is the runner's own count, never a protocol field.

**The runner counts from its store.** `yad daemon` reads the harnesses of the
runs held (`runner.Monitor.HeldHarnesses`) at every rebuild of the document,
after the probe. A claim recorded and a run ending do not wait for the next
probe: each recounts at once, recomputing the strings over the reports already
probed rather than probing again, so a queue of short runs spawns no harness's
`--version` once per run. The claim's recount is what covers a claim checked
against the document before a probe found its harness gone, and recorded
after that probe's rebuild read the runs held. Once a harness's last run has ended, the next document is computed
over the drivable harnesses alone, as the first amendment made it. A held run
moves the strings with the reports under the one fingerprint, as a harness
coming or going does, so a hub re-reads both together.

The price the first amendment named is paid: the document now depends on
run state as well as on the machine. It is small: a drivable harness counts
already, so a run starting or ending moves the strings only when its harness
has stopped being drivable while the run was held. The cost of the other
rule was a hub offering a control that fails with nothing to say why.
`yad harnesses` and the document `yad connect` registers with count none
held: neither process holds a run, so they show the drivable-only strings,
which is also what the daemon sends whenever no harness has gone from under a
run.

## The conformance suite

The suite's runner used to advertise no feature at all. It now advertises
`steer`, `interrupt`, `effort` and `fork` runner-wide beside
`harness_features` and an empty list for its one harness: a document saying
runs on that harness may use none of them. `run/gated-features` and
`versioning/controls-are-gated` then fail a hub that offers an effort or a
fork, or sends a steer or an interrupt, going by the runner-wide strings. No
yad runner sends that pairing — its runner-wide strings only ever say what
every harness supports — which is what makes it a probe: it is the one
document on which a hub that reads the lists and one that does not behave
differently.

That fails a hub which gates correctly on the runner-wide strings alone, and
against a yad runner such a hub is safe. It is failed anyway because HUB.md §7
now says the list is the answer when `harness_features` is advertised, and a
suite that could not tell the two apart would check nothing new.

## Considered options

**An admission rule instead of lists** — keep the runner-wide strings and
make a harness that lacks a feature refuse runs using it. The strings would
then lie for that harness, and a hub believing them would offer runs that are
refused on arrival and go on being offered.

**`fork:claude`-style strings** in `protocol_features`, as 0065 considered.
Parses a structure out of a string list, and every hub must learn the syntax;
a field on the harness report is where a hub already looks for what a harness
is.

**A non-`omitempty` list** so that an empty one could be told from an absent
one without a feature string. It would be the one list in the document that
behaves differently, and a hub generated from the OpenAPI document cannot see
the difference between `[]` and absent in most languages anyway.

**Runner-wide strings over what is installed** rather than over the build's
catalog. More generous to older hubs on a machine without the harness that
lacks a feature, at the price of the strings changing when a harness is
installed — which a hub reading only them would see as the runner losing a
feature mid-life. The build's catalog keeps the meaning they had.
*(Chosen after all by the amendment above: the catalog rule cost every
runner its runner-wide steer the moment one harness without it became
first-class.)*
