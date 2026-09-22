---
date: 2026-09-19
status: amended by 0047 — only a claim the hub never acknowledged is withdrawn, with its session; an acknowledged claim that never began is reported lost and keeps its session
---

# A restart reports its orphans lost, and replays what is owed before it claims

Epic E2 left a restarted runner giving up the runs its previous process held:
lost locally, dropped from the sync listing, and lost on the hub only once
their leases lapsed — a minute or more with no word, and no result naming the
events that were streamed. DEV-14 asks for more.

**An orphan is reported lost, never run again.** On start, every run the store
holds in a non-terminal state belongs to a process that is gone. Its harness
was that process's child, in a process group it killed on the way out; nothing
here can reattach to it, and running it again could do its work twice. So its
state becomes `lost` and a `lost` result goes into the outbox in the same
transaction, with error class `runner_restarted` and `last_seq` the last event
it spooled. Like any run with a result owed, it stays listed until the result
lands (decision [0023](0023-lost-stands-against-a-late-result.md)), and the
result waits behind the run's spooled events, so the hub holds the whole stream
when it learns the run ended. A hub that lost the run first answers 409, which
agrees. Re-offered, it is refused: a run is never run twice.

**A claim that never began is withdrawn, not lost.** A run still `claimed` —
recorded when offered, and never prepared — may never have been listed back:
the process can die in the round trip between the offer and the listing, and
the hub then still has it as offered. Such a run is removed, with the empty
session it opened, and left out of the listing, so the hub offers it again;
one the hub had acknowledged lapses into lost on its side. Reporting it lost
here would end, for good, a run nobody ever started.

**What is owed goes out before new work comes in.** Every connection settles
its orphans before any reporter starts, and its sync loop claims nothing until
its reporter's first flush — the replay of the spool and the outbox — has run.
Syncs go on meanwhile, with zero free capacity, so leases renew; and the loop
syncs again the moment the replay is done, so a restart costs no interval. A
hub out of reach fails the flush and the sync alike, so the gate does not wait
for a hub to come back.

**Sessions survive.** The native session id is written when the harness first
reveals it, and the workdir when it is made; a restart touches neither, and the
session stays open. The next run the hub sends in that session resumes the
conversation (`--resume <native id>`) in the same directory.

## Considered options

**Resume the orphan.** A harness process cannot be reattached, and a new one
resuming the session would redo the turn — the retry DOMAIN.md forbids. A
**waiting** run holds no process and will survive a restart when epic E6 makes
one; until then there is none, and every orphan is lost.

**Kill a surviving harness by its recorded process group.** A runner killed
with SIGKILL leaves its harness running until it writes to a closed pipe. A
group id recorded before the crash may name someone else's processes after a
reboot or a long uptime; killing it would be worse than the leftover. Left
open.

**Stay silent and let the lease lapse**, as E2 did. Simple, and the hub learns
late, without the run's last sequence number, and cannot tell a restart from a
network loss.
