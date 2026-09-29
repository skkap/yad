---
date: 2026-09-29
---

# A run ending brings its connection's next sync forward

A run queued behind one a runner holds starts at the runner's next sync after
the one ahead of it ends ([0005](0005-pull-by-periodic-sync.md)). `yad hub`
shortens that wait to 3 s by asking such a runner back sooner
([0063](0063-a-hub-holding-work-for-a-runner-asks-it-back-in-3-s.md)); any
other hub leaves it at its interval, 15 s by default. The runner is the one
that knows the moment a run ends, so it closes the gap for every hub, whatever
the hub answers (DEV-145).

**When a run a connection's loop started gives its capacity back, or that
connection's reporter has handled a result — the hub has it, never will, or
it waits for a retry — the loop syncs early** — at once, or
`earlySyncGap` (1 s) after its last sync if that was sooner. The hub's
interval is still the gap between ordinary syncs. Nothing on the wire
changes.

- **One signal, per connection.** The wake is a one-slot channel on the loop,
  emptied as each sync begins: every run that ends before that sync reads the
  store is answered by it, so ten runs ending together are one early sync, and
  another connection's loop is not woken for a run it does not hold.
- **Not before the result is in.** A hub learns a run ended from its result,
  not from a sync: until then the sync lists the run as running, so the
  session's next turn is not offered and the early sync is spent. A wake while
  one of the connection's results is due is let go, and the reporter's wake
  once it is handled is the one that syncs. A result in backoff does not hold
  it: that hub is failing, and it would hold the early sync until the hub came
  back. So a delivery that fails wakes the loop as well.
- **Not before the capacity is back.** The executor writes a run's result,
  then gives its capacity back, and the reporter may deliver the result in
  between — on a loaded machine it does (DEV-156). A sync then lists the run
  as ended but offers no capacity for the next turn, so the hub hands it to
  nobody and the early sync is spent. A wake while a run the loop started has
  ended in the store with its capacity still out is let go too; the release's
  own wake is the one that syncs.
- **Not while a hub is failing.** During the error backoff no wake is heard:
  the backoff is what spares the hub.
- **`earlySyncGap` is 1 s.** It bounds a queue of runs that each fail the
  moment they start, which would otherwise sync as fast as the hub answers. A
  runner already calls its hub about every second while a run streams — the
  reporter's tick — so this adds nothing of a different order, and it is under
  the 3 s floor a hub may ask for, so bringing a sync forward is never slower
  than waiting for it.

Every way a started run gives its capacity back — a result, a park on a usage
limit, a stop — goes through its claim's release, so a run that ends with no
result to report brings the sync forward too, and the hub hears a park at once.

## Considered options

**Waking on the release alone** — simpler, and with the result typically still
on its way the sync it brings lists the run as running and is wasted; the next
turn then waits for the next one after it.

**Waking every connection** — a unit a run frees on one connection is dealt
round the ring ([ARCHITECTURE.md §2](../../ARCHITECTURE.md)), so another
connection might take it; but that connection's hub has nothing new to say
about its own runs, and a burst of ends on one hub would sync every hub.
The others take the unit at their own next sync, as before.

**No gap** — a failing queue turns into a tight loop against the hub,
bounded only by its answers.
