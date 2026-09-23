---
date: 2026-09-23
---

# A run's effort is the harness's word

Zumino's automations carry an effort (ZUM-83), and v1 had no way to pass one:
a hub could choose a harness's model and not how hard it thinks. Claude Code
takes `--effort low|medium|high|xhigh|max`; Codex takes a reasoning effort per
turn, from a set each model lists (`low` to `xhigh` for most, `max` or
`ultra` for some). DEV-124 added it. The contract was decided with the owner;
what is recorded here is why it has this shape, and the two things the
harnesses did that the contract had to absorb.

## A string, not an enum

**Decision.** `Run.effort` is an optional string in the harness's own terms,
as `model` is. Absent is the harness's default. The runner checks no name and
hands the word over: Claude's `--effort`, and the `effort` of Codex's
`turn/start` in the pinned app-server protocol
([0037](0037-a-codex-run-is-its-own-turn-and-its-protocol-is-pinned.md)).
A level the harness does not take fails the run with class `harness_error`
and the harness's own words.

**Why.** The levels are each harness's, they differ by model, and they grow
with harness releases — `xhigh` and `max` are recent in both. An enum would be
a v1 enum, closed for all of v1
([0047](0047-four-v1-protocol-rules-settled-by-the-clean-room-check.md)), so
every new level would need a feature of its own; and a runner that checked
names would refuse a level its harness had just learned. The harness already
answers the question precisely: Codex's refusal lists the levels its API takes.

**Considered.** A normalised scale (`low`/`medium`/`high`) mapped per
harness — a third vocabulary matching neither harness, and it would hide
`xhigh`, `max` and `ultra`. Validating against a list per harness in the
runner — a second copy of each harness's list, wrong the day either ships.

## Gated by a feature, with no expiry

**Decision.** A new protocol feature, `effort`, advertised by a runner whose
every first-class adapter applies a run's effort. A hub offers a run carrying
one only to a runner advertising it — like `start_at`, except that it never
lapses: the run waits for such a runner however long that takes. `yad hub`
does exactly that. A runner advertising the feature that is handed an effort
for a harness whose adapter cannot set it refuses the run, class `refused`,
rather than run at the default; with both adapters setting it, that path
exists only for a harness made first-class later, and a test fails if one is
made first-class without it.

**Why.** A runner that predates the field drops it as an unknown field and
runs the harness at its default. The run succeeds and nothing in its result
says it ran at another effort than the one asked for — the failure the
feature mechanism exists to prevent. A `start_at` stops mattering once its
moment has passed; an effort never does, so there is no point at which
offering it anywhere becomes right.

**What it costs.** An effort run on a fleet with no runner advertising the
feature waits for ever, and so does one continuing a session bound to such a
runner. HUB.md §4 says to tell the submitter or resubmit without the effort.

## Claude ignores what it does not know

**Decision.** Claude Code 2.1.280 does not refuse an unknown `--effort`: it
prints `Warning: Unknown --effort value 'bogus' — ignoring it and using the
default effort. Valid values: …` on stderr and runs the turn, exit 0. The
adapter reads stderr beside Claude's first lines of output, and on that
warning stops the turn and fails the run with class `harness_error` and the
warning as the message. The outcome looks at the whole stderr tail again, so a
warning the first look missed still fails the run.

**Why.** Left alone, the run would succeed having done its work at an effort
nobody asked for — exactly what the gate prevents for older runners. The
warning is Claude's verdict, not the runner's: the adapter reads that Claude
refused a level, and never which levels exist. Stopping at the first output
means the turn has done nothing yet; failing only at the end would throw away
work already done, commits and pushes included.

**Considered.** Validating Claude's levels in the adapter — a copied list,
the thing the first section rejects. Failing only at the end of the turn —
simpler, and the turn would already have run.

## An effort does not outlive its run

Codex's `turn/start` takes an effort "for this turn and subsequent turns",
which read as though a later run in the same session, sent without one, would
inherit it. Measured on Codex 0.147.0: it does not. Each run is its own
app-server process, and a resumed thread's next turn, after a run that set
`low`, recorded no effort in Codex's own rollout — the model's default.
Claude's `--effort` is per process too. So absent means the harness's default
on every run, as the field's description says, and a hub that wants one
effort for a whole session sends it with every run.

## Codex reports its models

In the same change, `HarnessReport.models` for Codex is read from the model
list Codex caches in each home a run may use (`models_cache.json`): every
account's home, or the default home when there are no accounts. Only models
Codex shows in its own picker, in its order, and only names shaped like
model names — the capability document reaches every hub (DEV-67). The cache
also holds each model's supported reasoning levels and default; the list of
names stays the documented field, and a per-model structure for the levels is
proposed in the pull request rather than added here.
