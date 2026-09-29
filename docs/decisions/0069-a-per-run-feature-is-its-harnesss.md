---
date: 2026-09-30
---

# A per-run feature is its harness's

Four protocol features are about what a run may do rather than what the
runner does: `steer`, `interrupt`, `effort` and `fork`. Until now each was one
runner-wide string, advertised only while every first-class adapter
supported it (decisions 0049, 0065), with a test holding the two together.
That was fine while Claude and Codex were the only harnesses and both did all
four. The next harnesses are driven over the Agent Client Protocol (DEV-44),
and ACP has no steer at all; Copilot, Cursor and Gemini have no fork. Made
first-class under the old rule, any one of them would switch the feature off
for Claude and Codex runs on every runner of that build. The owner decided on
2026-09-30 that the features become per harness first. DEV-158.

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
detection.

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
