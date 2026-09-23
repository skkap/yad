---
date: 2026-09-24
---

# A run's context reaches the harness on every run

HUB.md promised that `brief.context` is appended to the harness's system
prompt and survives compaction. Zumino's hub found it true only for a
session's first run (DEV-122, from ZUM-75): a question put only in a
continuing run's context went unanswered, and an automation's standing
instructions, which Zumino puts in context, were obeyed on a session's first
run and ignored on its continuations.

## What the harnesses did

Reproduced through `yad hub` on Claude Code 2.1.280 (haiku) and Codex 0.147.0
(gpt-5.6-luna). A session's first run had context `Be brief.`; its second had
`The codeword is BLUE. If asked, give the codeword.` and the instruction `What
is the codeword?`. Both harnesses answered that they knew no codeword. A
context kept the same across runs (`Always answer in French.`) seemed to work,
but only because the first run's was still in force.

- **Claude** was passed `--append-system-prompt-file` with the new context on
  `--resume`, and ignored it — the string form too, and `--system-prompt`.
  Claude records a conversation's system prompt on its first request and
  sends that record on every later request and resume, "even when a later
  launch passes different text, until the conversation is compacted"
  (`claude --help`, `--system-prompt-snapshot`). A resumed run with no flag
  at all still answered from the first run's appended prompt.
- **Codex** was passed the new context as `developerInstructions` on
  `thread/resume`. It keeps them as the thread's, but the model reads the
  developer message Codex wrote when the thread started; the new
  instructions reach it only when a compaction rebuilds the thread's opening.
  After a `thread/compact/start` the same resumed thread answered BLUE.

## Decision

The context is each run's standing background, and the adapters make it so.

- **Claude**: every run passes `--system-prompt-snapshot off`, so the system
  prompt — this run's context appended — is rendered afresh on every request,
  after a compaction too.
- **Codex**: `developerInstructions` still goes on `thread/resume`, which
  covers the thread after a compaction; and on a resumed run with a context,
  the adapter also appends it to the thread as a developer message with
  `thread/inject_items`, between the resume and the run's `turn/start`. The
  method and its answer join the pinned surface (0037); the 0.147.0 schema is
  unchanged, and its surface hash moves with the surface.

HUB.md §4 now says what context means on a continuing run, and what a hub puts
in context rather than in the instruction: what every run must obey or know,
sent whole on every run; the instruction is only this run's request.

## Why this and not the others

**Repeating the context in the user turn** — a marked preamble before the
instruction — works on every version of both harnesses, and is what the ticket
first proposed. It makes the hub's standing rules part of the conversation,
where the harness weighs them as something a user once said, and where the
next compaction may summarise them away mid-run. Both harnesses have a place
for standing instructions outside the conversation; the fix uses it.

**Saying in HUB.md that context applies only when a session opens** would
have put the harnesses' quirk in the protocol, and left every hub to fold its
standing rules into each instruction.

**Claude's snapshot left on, with the flag passed only when the context
changes** — the runner would have to remember each session's last context to
know, for no gain: with the prompt's text the same on every request of a run,
the prompt cache still holds (measured: a resumed turn with the snapshot off
read 23.8k tokens from cache and wrote 328).

## What it costs

- **A Claude that does not know `--system-prompt-snapshot`** fails every run
  at its arguments. Every installed version checked, 2.1.276 to 2.1.280, has
  it; an older one's run fails saying to upgrade Claude Code rather than to
  log in again.
- **A Codex thread keeps each resumed run's context in its history**, one
  developer message per run. They are short and the latest is the one in
  force; a run does not erase what an earlier one said, and HUB.md says so.
- **A run without a context** gets none of its own: Claude's system prompt
  then carries no appended text, and Codex injects nothing. A hub that wants
  a rule to hold for a whole session sends it with every run.
