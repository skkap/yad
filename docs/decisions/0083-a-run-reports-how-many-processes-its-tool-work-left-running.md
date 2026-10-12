---
date: 2026-10-12
status: amended by 0086 — an owner may turn on stop_left_running, and yad then stops what the census counts as it counts it
---

# A run reports how many processes its tool work left running

When a harness's turn ends, the work its agent started in the background — a
test suite, a dev server started with `&` or Claude Code's
`run_in_background` — goes with it, and the result still said `succeeded`,
with a final text that may say "tests are running, will report". In
JapanLaw #1 (ZUM-100) a manager trusted two such builder runs and caught it
only by reading transcripts. A hub had no way to tell a hollow success from a
real one. YAD-186.

## Decision

The owner shaped this on the ticket: the name and its meaning, and when the
count is taken. The rest was settled by measuring the three harnesses.

**`metrics.left_running`** is how many processes the run's tool work still had
running when a turn ended on the harness's own answer, whoever stopped them
after. It is optional. Absent means not measured: every turn was cancelled,
interrupted or stopped by a watchdog, or the process table could not be read.
`0` means measured, with nothing found. It is summed over the turns that were
measured, as `tool_calls` is over every turn. The ticket's
`descendants_killed` was the first name, and it would have been false: yad
does not kill these processes (below), the harness does.

**A `left_running` status event names them**, before the result, so the
result's `last_seq` covers it: each by the kernel's name for its command and
how long it had run — `1 process the run's tool work started was still
running when the turn ended: sleep (running 4s)` — at most 20, then "and N
more". Only when something was found. Never argv, never environment, never a
pid: a command line can hold a token (0015), and a pid means nothing off the
machine. The name is `ucomm` on macOS, whose `comm` is argv[0], and `comm`
on Linux, the kernel's task name; both are the executable's file name cut to
16 or 15 bytes.

**The census is taken by each adapter after the harness's own end of the
turn and before its input closes**: Claude in `settle`, when the result in
hand answered every frame sent; Codex at `turn/completed`; the ACP core when
`session/prompt` is answered. After that the harness is told to go, and stops
its tool processes on the way out — and once it has exited its children
belong to init, where nothing marks them as its. One `ps` per turn end,
through `supervise.Run`, bounded at 5 s; the leader is yad's unreaped child
throughout, so its pid cannot have been reused, and a leader that exited
around the listing makes the figure absent rather than 0.

**What counts**: every live process in a process group of its own started by
a process in the harness's group, and every descendant of one of those
wherever it went (a `setsid` grandchild), minus

- **a shell only waiting on its command** (`sh`, `bash`, `zsh` and the like,
  with a live child counted): Claude runs a background command as
  `zsh -c '… sleep 600 …'`, and that is one thing left running, not two;
- **named helpers** of the harness: Codex's `codex-code-mode-host`, which it
  starts beside a turn's commands in a group of its own.

The harness's own group is left out: it holds the harness's helpers (Claude's
stdio MCP servers, measured there), which cannot be told from tool work by
name. A tool's group is found from any process in it, not from the leader
alone, because the leader may be a launcher: npm's `codex` is a node script
that runs the native binary as its child, in its own group, and a census
keyed on the leader's children would read a measured 0 for every launched
Codex. The price is a helper in that group that starts a process in a group
of its own — an MCP server that does is counted.

**yad does not stop the survivors.** Each harness measured stops them itself
as it exits. Stopping, or keeping, work a run left behind is a separate
question and a separate ticket. *(Amended by [0086](0086-the-owner-may-have-yad-stop-what-a-turn-left-running.md):
with `[supervise] stop_left_running` on, yad stops what the census counted,
there and then; off, the default, this holds.)*

No feature gate: the field is optional and additive (oasdiff passes), and a
status label is an open set. The conformance suite now puts an unknown field
inside `metrics` as well as beside the result and inside its error, so a hub
whose generated `Metrics` type refuses what its schema lacks fails conformance
instead of refusing every result from a newer runner.

## Measured

2026-10-12, macOS 26 (Darwin 25.6), one `ps -A -o pid,ppid,pgid,etime,ucomm,comm`
taken at the harness's end of turn with its input open, again after it
exited. Each harness was started in a process group of its own, as
`supervise.Start` starts it, and driven over its own protocol as the adapters
drive it. Cheapest models: Claude `haiku`, Codex `gpt-5.6-luna`, OpenCode
`opencode/nemotron-3.5-lightning-free` on Zen, in a throwaway home.

| | harness | the tool call | at the end of the turn | afterwards |
|---|---|---|---|---|
| M1 | Claude Code 2.1.296 | Bash `sleep 601`, `run_in_background` | `zsh` (own session and group, child of claude) with `sleep` beneath it | claude did not exit when its input closed; yad's ladder SIGTERMs its group after 10 s, and claude then stopped the sleep itself |
| M2 | Claude Code 2.1.296 | Bash `nohup sleep 602 … &`, foreground | `sleep`, ppid 1, in the dead shell's group | claude exited at once; the sleep **survived** |
| M3 | Claude Code 2.1.296 | none; a stdio MCP server configured | the MCP server, a direct child **in claude's own group**; nothing else (no caffeinate) | gone with claude |
| M4 | Codex 0.160.1 | `zsh -lc 'sleep 604 &'` | nothing: Codex ended the command's group when the command returned; `codex-code-mode-host` in a group of its own | — |
| M4b | Codex 0.160.1 | a background terminal, `zsh -lc 'sleep 605'` | `sleep` (exec'd in place of zsh), own group, child of codex; `codex-code-mode-host` | codex exited at once and stopped the sleep |
| M5 | OpenCode 1.18.33 | `sleep 606 &` | nothing | — |
| M5b | OpenCode 1.18.33 | `nohup sleep 607 … &` | `sleep`, ppid 1, in the dead shell's group | opencode exited at once; the sleep **survived** |

So M1 counts 1 (`sleep`), M4b counts 1 (`sleep`), M3 counts 0, and the
others count 0. What M2 and M5b show is the reach limit: **a process whose
shell backgrounded it and then exited** is handed to init at once, in a group
whose leader is gone, and nothing on macOS ties it back to the run without a
subreaper. It is not counted — and unlike the rest it is not stopped either:
it outlives the turn. Linux's `PR_SET_CHILD_SUBREAPER` could reach it; that
belongs with the ticket that decides what yad does with survivors.

`ucomm` of Claude's own process reads `2.1.296`, the versioned file its
`claude` link points at; the census never needs the harness's name, since it
works from the harness's pid.

## Considered

- **Counting what yad's group kill reaps** (the ticket's first mechanism).
  Every harness measured starts tool commands outside its own group, so that
  kill never reaches them, and the count would be 0 in exactly the case the
  ticket is about.
- **Also counting what is left in the harness's own group at the reap,**
  recorded before the first group signal. Measured, that group holds only the
  harness's helpers, so it would add false positives — a Claude with an MCP
  server would report one on every run — and nothing the ticket asks for.
  Dropped; a harness that someday runs tool commands in its own group is not
  measured, and this record is where to look.
- **Tool groups led by a direct child of the leader** (the first cut, and
  what the measurements show for a native binary). Review found the launcher
  case above: a measured 0 for work that was left running.
- **Counting every process, wrapper shells included.** A Claude background
  command would count 2 for one `sleep`, and every hub would learn to halve
  it.

## Consequences

- Tied to the versions above. A harness release that moves its tool commands
  into its own group makes them invisible to the census (the figure reads 0),
  and one that adds a helper beside them makes it read high until the helper
  is named here and in its adapter.
- One `ps` per turn that ends on the harness's answer; a turn that ends any
  other way costs nothing.
- Helpers not measured — an OpenCode LSP server, a plugin's daemon started in
  a group of its own — would be counted. The event names them, so a hub and an
  owner can see it.
