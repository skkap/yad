// Package conformance is the v1 protocol conformance suite: the rules of
// ARCHITECTURE.md §2 as black-box checks against a hub URL, so a hub written
// in any language — Zumino's embedded one, a hub someone builds from §2 and
// protocol/v1/openapi.yaml — can prove it speaks the protocol before a runner
// ever connects.
//
// It holds a URL and a registration token and nothing else. Nothing here
// imports internal/hub: a suite that reached into `yad hub`'s types would be
// testing this implementation rather than the protocol, and the hubs it exists
// for would not be checkable by it. `yad hub` is checked against it like any
// other hub, over HTTP on a local listener, in internal/hub's own tests.
//
// A failure names the rule in a sentence and the part of §2 it is written in,
// because the person reading it is implementing a hub and does not have this
// repository open. "expected 409, got 200" tells them nothing.
//
// What it does to a hub. It registers a runner — advertising DefaultHarness,
// which no real run asks for, so a suite pointed at a working hub is offered
// nothing anyone was waiting on — and burns the registration token it was
// given. The rules about claiming, events, results and the lease need a run:
// queue one or two for that harness, and the suite claims them, reports one
// failed with error class refused (§2's answer for a run a runner will not
// take) and leaves the other to lose its lease. Without them those checks are
// reported as skipped, with what to queue to make them possible: a skip is
// never a pass.
//
// Epic E7 (Zumino yad/dev).
package conformance
