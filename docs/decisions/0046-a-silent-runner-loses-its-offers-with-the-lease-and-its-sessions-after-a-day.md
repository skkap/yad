---
date: 2026-09-22
---

# A silent runner loses its offers with the lease, and its sessions after a day

A runner that deregisters is settled at once: its held runs are lost, its
offers requeued, its sessions closed with the runs queued in them ended
(DEV-77). A runner that simply stops syncing — crashed, switched off, cut off —
was settled only for the runs it held, whose leases lapse. Two things were left
to each hub to invent (DEV-94): when it may take back an **offer** the runner
never claimed, since re-offering was defined by a *next* sync that never comes;
and when it may give up the runner's **sessions**, whose later runs go to that
runner alone and so wait for good. A fresh agent writing a hub from HUB.md
alone invented a 20-second offer expiry, because the document could not give
it an answer.

## Decision

The owner's (2026-09-22).

**An offer expires with the lease.** The `lease_ms` in a sync answer covers the
runs it offers as well as the runs the request listed. An offer not claimed
within it goes back in the queue and may be offered to any runner; a claim
listed after that is answered with `cancel`, the same way as a claim whose
lease lapsed. No protocol shape changes: `yad hub` already requeued a lapsed
offer (`RequeueWithdrawnOffers`), and this writes down what it did. `yad hub`
also leaves the cancelled run out of the offers in the same answer, so no
runner is told to stop and to start one run at once; it may be offered at the
next sync. Conformance checks it (`lease/offer-lapse`): a runner silent for the
whole lease that then lists the offer must hear `cancel`.

**A silent runner's sessions are given up after a long silence.** Each hub
names an abandon-after, longer than its lease; `yad hub` defaults to 24 hours
and takes `yad hub serve --abandon-after`, refusing a value no longer than the
lease. After that much silence, counted from the runner's last sync the hub
answered, the hub closes every session bound to it and fails the runs queued in
them with a reason naming the runner and saying to submit the work to a new
session. It does this through the same `abandon()` deregister uses, with its
own departure, and **does not retire the credential**. The runner may only
have been switched off: when it syncs again it is answered normally, hears
`close_session` for each of those sessions until it lists the session in
`closed_sessions` ([0011](0011-hub-closes-sessions-runner-collects.md),
[0035](0035-a-runner-reports-every-close-in-its-sync.md)) — so it reclaims the
workdirs — and takes new work. Conformance cannot check this: it cannot wait
out a length each hub chooses and v1 gives it no way to learn, so the rule is
on the suite's not-checked list.

`yad hub` does not count time it was itself down. Silence starts no earlier
than the hub's own start, so a hub that comes back after a weekend gives every
runner a full abandon-after to sync before it closes anything. That was not in
the owner's decision; without it the first sweep after a long outage closes
every session in the fleet for a silence the hub caused.

## Considered options

**A short offer expiry of its own** — the 20 seconds the clean-context hub
invented. A second timing a hub must name, a runner cannot learn, and nothing
gains from: the lease already says how long the hub will wait on a runner.

**Offers never expire; a hub waits for the runner's next sync.** Faithful to
claim-by-listing, and a run offered to a machine that was switched off waits
until it is switched on again, however long that is.

**Give up the sessions when the held runs' leases lapse.** The same moment
decides everything, and a laptop closed for a coffee loses every warm session
it had. Sessions are worth keeping far longer than a run is worth waiting on,
which is why the silence that closes them is a day and not a minute.

**Retire the credential too, as deregister does.** A runner cannot tell a
silence it caused from a network fault it did not; retiring its credential
turns a switched-off machine into one that needs a new registration token.
Deregister is the runner's own statement that it is not coming back. Silence is
not.

**Unbind the sessions instead of closing them.** Rejected for deregister
already: a session is resumable only on the runner that holds it, and a resume
offered anywhere else cannot work.
