---
date: 2026-09-18
status: superseded in part by 0003 — the generic core stands; "one control plane per runner" and per-control-plane drivers do not
---

# A generic runner protocol, with Zumino as the first driver

The fastest useful thing would have been a Zumino agent: PAT, poll `GET /queue`,
pull `…/tasks/{n}/context`, run `claude`, write the spec half, move the status.
Days, not weeks, and no protocol to design. It was rejected because the second
caller already exists — yashiki needs the same runner for assistant work that has
no task, no queue and no ref — and because the things Zumino would have to grow
to host this (capability advertisement, liveness, session affinity, an executor
registry) are precisely the things its own README says it deliberately does not
have. Bolting them onto a tracker makes the tracker worse and the runner
unusable anywhere else.

So the claim boundary is the seam: everything below it — identity, capabilities,
sessions, supervision, cancel, streaming — is control-plane agnostic, and a
**driver** adapts one control plane above it. Zumino's driver ships first
because it has real queued work to execute.

## Considered options

**Fork multica's daemon** and point it at a private server. It would have run
fourteen agent CLIs on day one. Rejected on ownership: it brings Multica's task
model, its licence terms and a permanent merge burden, for a system whose two
consumers are already written and disagree with that model.

**Be a Zumino agent only** — see above. Still the right answer if yashiki were
not real.

## Consequences

A driver interface has to be honest from M3 rather than discovered at M4, and
"one control plane per runner" (`DESIGN.md §1`) is what keeps it honest — a
runner serving two queues would need concurrency arbitration, and that is a
distributed-scheduling problem this project is not going to be good at. Two
control planes means two runners.
