---
date: 2026-09-18
---

# Go, not Rust

`gpi-tools` is Rust and this would have been the natural fourth CLI in that
family. YAD is Go anyway, for one reason that outweighs house style: the hard
part of a runner is not the daemon, it is the per-CLI adapters, and ~14,000 lines
of exactly those adapters already exist in readable Go at
`~/projects/multica/server/pkg/agent` under a modified Apache-2.0 licence —
streaming JSON parse per agent, ACP for the CLIs that speak it, process-tree
handling on Unix and Windows, inactivity watchdogs, cancel semantics that
actually stop a child `npm`. Being able to read that while writing this, and to
lift shapes from it with attribution, is worth more than language preference.

The second reason is the deployment: one static binary, `GOOS=linux
GOARCH=amd64`, copied onto a box with no toolchain and no runtime. Rust does this
too; Go does it with a cross-compile that never needs a linker from the target
platform, which matters when the laptop is macOS/arm64 and the fleet is not.

## Considered options

**Rust.** Matches `gpi-tools`, gives a smaller binary and stronger guarantees
around the process and signal handling that this program is mostly made of.
Rejected because every adapter starts from zero, and adapters are the whole
product. If YAD ever stops being adapter-bound — if ACP makes one generic
adapter sufficient — this decision is worth revisiting rather than defending.

**TypeScript**, matching yashiki, so the two could share types. Rejected: a
runner that needs a Node install on every machine it is copied to is not the
boring artefact this is supposed to be.

## Consequences

`gpi-tools` stays Rust and YAD is the one Go repo; that split is accepted rather
than resolved. The standard-library-only rule in `DESIGN.md §6` matters more in
Go than it would in Rust, because Go's ecosystem makes `go get` frictionless and
this binary holds tokens on machines that run agents with filesystem access.
