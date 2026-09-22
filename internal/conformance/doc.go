// Package conformance is the v1 protocol conformance suite: the rules of
// HUB.md as black-box checks against a hub URL, so a hub written
// in any language — Zumino's embedded one, a hub someone builds from HUB.md and
// protocol/v1/openapi.yaml — can prove it speaks the protocol before a runner
// ever connects.
//
// It holds a URL and a registration token — two, for the holder rules — and
// nothing else. Nothing here
// imports internal/hub: a suite that reached into `yad hub`'s types would be
// testing this implementation rather than the protocol, and the hubs it exists
// for would not be checkable by it. `yad hub` is checked against it like any
// other hub, over HTTP on a local listener, in internal/hub's own tests.
//
// What it will not print. The report is written to a terminal and kept in
// whatever log ran it, so nothing in it is a secret: the registration token
// and the credential this suite was given are removed from every sentence and
// every body, a register answer that succeeded is described rather than shown
// because its credential may be under a name only that hub knows, and the
// secrets a hub *sends* — a grant's value, a password inside a run's source
// URL, credentials in the connection URL — are removed too, by learning them
// or by taking them out of the URL.
//
// The bound on that: a hub is free to write anything into an error message, an
// error code or a run id, including a secret belonging to some other run this
// suite was never offered and cannot know. Nothing here can catch that, and
// nothing here pretends to — what this guards is every secret the protocol
// carries and every secret the suite has held.
//
// A failure names the rule in a sentence and the section of HUB.md it is written in,
// because the person reading it is implementing a hub and does not have this
// repository open. "expected 409, got 200" tells them nothing.
//
// What it does to a hub. It registers a runner — advertising DefaultHarness,
// which no real run asks for, so a suite pointed at a working hub is offered
// nothing anyone was waiting on — and burns the registration token it was
// given. The rules about claiming, events, results and the lease need a run:
// queue three for that harness, and the suite claims two, reports one failed
// with error class refused (HUB.md's answer for a run a runner will not take),
// leaves the other to lose its lease, leaves the third offered and unclaimed
// until its offer lapses, and then declines that one without claiming it —
// a failed result, class refused, for a run only offered. It also sends one
// events batch and one result of 17 MiB each, to see how a hub refuses a body
// for its size. Without the runs those checks are
// reported as skipped, with what to queue to make them possible: a skip is
// never a pass. Given a second registration token, it spends that too, on a
// second runner that sends events and a result for the run the first one
// holds — which the hub must refuse.
//
// Epic E7 (Zumino yad/dev).
package conformance
