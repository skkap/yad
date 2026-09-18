---
date: 2026-09-19
---

# Drain is a ladder of three signals, and a draining runner says so in its health

A runner is stopped for an upgrade, a reboot or a retiring machine, and each of
those should cost no run that was about to finish. DOMAIN.md defines **drain**
— stop claiming, let live runs finish, then exit — and DEV-13 put it on the
signals a runner already gets.

**Three signals, counted.** The first stop signal (SIGTERM or SIGINT) drains.
The second cancels every run held, down the cancel ladder of ARCHITECTURE.md §3
— interrupt, SIGTERM to the group, SIGKILL — and the runner exits once each has
its result. The third exits at once: runs in hand are killed where they stand,
and the next start reports them (decision
[0030](0030-a-restart-reports-lost-and-replays-first.md)). Signals are counted,
not taken as "the next step from where the runner is": a runner the hub is
already draining still only drains on a service manager's SIGTERM, and a
polite stop never cancels someone's run.

**The drain wait bounds the first step.** `[drain] wait` in config.toml
(default 30 min, the same as the inactivity watchdog, and for the same reason:
cutting short a healthy run throws its work away). When it runs out the runner
takes the second step by itself: runs are cancelled rather than abandoned,
because a cancelled run has a result now and a session left resumable, while
an abandoned one is lost only once its lease lapses. `"0s"` cancels at once.

**A draining runner keeps syncing.** It lists every run it holds, so leases
renew and results land, and it exits only once every run has ended and — for
up to 30 s — the spool and the outbox are empty. Whatever is still owed then is
replayed at the next start. Runs it was offered and never listed are withdrawn
and left out of the listing, so the hub queues them again. A runner stopped
while a run's cancel is under way keeps the cancel's verdict: the run ends
`cancelled`, not `lost`.

**A cancel on the way down says who did it.** The result is `cancelled` with
error class `runner_stopping` and the reason, so a hub can tell an owner
stopping their machine from a person pressing cancel. Error classes are open
strings in v1, so this is an addition.

**Draining is both a health field and a feature.** `health.draining` is what a
hub routes on, sync by sync: `true` means free capacity zero and no offers, and
it is the runner's own word, so it holds for a drain started by a signal as
much as by the hub. The capability document's `drain` feature says the runner
acts on the drain control and reports the field; a hub sends `drain` only to a
runner that advertises it. Both are additions within v1 — a field older hubs
ignore and a feature string older runners never send.

**The hub's drain control is repeated until the health answers it.** Controls
are not acknowledged (decision
[0025](0025-a-cancel-is-repeated-and-an-answer-that-landed-stands.md)); `yad hub`
keeps the request on the runner (`drain_requested_at`), sends `drain` in every
sync response and offers nothing until a sync says `draining`, then clears it.
Cleared, it cannot drain the runner's next process too. The service API is
`POST /api/v1/runners/{runner}/drain` and the CLI `yad hub drain <runner>`.

**One seam for every way down.** `runner.Drain` is the state: `Begin` for the
first step, `Cancel` for the second, and the end of Serve's context for the
third. Signals, the hub's control and — with DEV-10/11 — the control socket's
`yad daemon stop` all go through it.

A service manager's stop timeout must exceed the drain wait plus the cancel
ladder (about 15 s), or it kills the runner mid-drain and its runs end `lost`
at the next start. So `yad service install` derives the units' stop timeout
(`ExitTimeOut`, `TimeoutStopSec`) from the drain wait — `runner.StopBudget`:
the wait, the ladder, the last flush and 15 s of slack — instead of the flat
30 s decision 0028 chose before drains existed, which stays the floor. A unit
holds the timeout it was rendered with: a changed drain wait takes a
`yad service install` again, and that reinstall itself drains the running
runner first.

## Considered options

**Exit at the end of the drain wait, abandoning the runs.** What "then exit"
says literally. Every such run would be lost a lease later with no result,
where a cancel reports it at once and leaves its session resumable.

**Draining as a new run state or a capability change only.** A capability
document moves rarely and is sent only when the fingerprint moves, so a hub
would learn of a drain late; a run state describes runs, not runners. Health is
sent on every sync and is already what hubs route on.

**A drain acknowledgement field in the sync request.** Exact once, and a field
every hub must track; the health field already says it, with no new field.
