---
date: 2026-09-28
---

# YAD is MIT-licensed

YAD goes public so that anyone can read the code before running it on a machine
that holds their tokens, install it without access to a private repository,
and build a hub against it. The licence has to make those three things free of
friction, and it has to leave Zumino — the first hub, and a commercial one —
free to embed the protocol without a second licence. MIT does all of that with
the fewest words, and every module linked into the binary is already MIT or
BSD, so the binary carries no condition stronger than its own.

The one obligation MIT and BSD place on a binary distribution is that the
copyright notices travel with it. A release carries them as
`THIRD_PARTY_LICENSES.txt`, built by `make dist` from the modules linked into
`cmd/yad`.

## Considered options

**Apache-2.0** — the same freedoms plus an explicit patent grant from every
contributor, and a clause that keeps the name out of the grant. Worth it for a
project where companies land code; with one author and no patents it buys
little, and it asks more of anyone who redistributes a modified copy. Moving to
it later stays possible while the author holds the copyright of every line.

**AGPL** — its network clause barely touches a client-side runner, and it
deters exactly the companies that would run one on their machines.

**FSL or BSL** — they guard against someone reselling the runner, which is not
a threat worth the trust they cost; pkg.go.dev does not display modules under
them either.
