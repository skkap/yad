---
status: amended by 0039 — the soonest-resetting free account goes first, login is the harness's own, and the terms question is withdrawn
date: 2026-09-18
---

# Accounts fail over in owner order; a limited run waits

A runner can hold several **accounts** per harness, in the owner's order, each
with its own harness home. A new run takes the first account that is not at a
usage limit. A run that hits a limit mid-turn moves to the next free account at
once; only when none is free does it become **waiting** — no process, a reason
and a resume time reported in every sync, persisted so it survives a restart —
and it continues the same session with a continuation turn once a limit resets.
A hub may cap the wait, after which the run times out. While every account of a
harness is limited, the runner stops claiming for it.

This is the behaviour the operator missed most in Multica, where a limit is a
failure the hub has to notice and redo.

Sessions move between accounts because each harness's transcripts live in one
shared directory linked into every account home. Measured on both CLIs — see
the note below.

## Considered options

**End the run as limited and let the hub resubmit** — a simple runner, and every
hub reimplementing wait-and-resume. **The hub picks the account** — puts billing
in the hub's hands, and account names into the protocol.

## Consequences

Using several subscriptions is the owner's decision
([0039](0039-accounts-log-in-themselves-and-the-soonest-reset-goes-first.md)).

## Verified by experiment, DEV-24

Run by hand on 2026-09-19 against **claude 2.1.278** and **codex-cli 0.147.0**.
The result is pinned to those versions: it rests on where each CLI keeps a
transcript and when it reads one, and either could change that in a release.

Two homes per harness, each with the transcript directory linked to one shared
directory — `projects/` for Claude, `sessions/` for Codex. A session was created
in the first home and resumed from the second.

- **A home that never created the session finds it.** Both CLIs resolve
  `resume` before they check the credential, so the lookup was measured against
  an unauthenticated home: an id present in the shared directory reached the
  auth gate, an absent one gave `No conversation found with session ID` and
  `no rollout found for thread id`.
- **The transcript is the whole of the session.** Each harness was told a word,
  then resumed through the shared directory by a different home, and answered
  with it, on the same native id. The resumed turn appends to the same file, so
  the shared directory stays the one copy.
- **The shared directory is the only one that has to be shared.** Codex writes a
  `threads` index in `state_5.sqlite` in `CODEX_HOME`, but it is a cache: a home
  with an empty database scanned the shared `sessions/` and rebuilt its own row.
  Claude's `session-env/<id>/` is empty and resume works without it.
- **Resume by id ignores the working directory.** Claude scans all of
  `projects/` rather than the subdirectory its cwd encodes; `codex exec resume`
  is likewise cwd-agnostic. Only Codex's interactive picker filters by cwd.

**Not measured: a provider refusing a continuation billed to a second account.**
No second login existed on the machine, and neither creating one unattended nor
copying a credential into a test home was acceptable. Tracked as **DEV-58**, so
the gap stays a task rather than a caveat in a merged document. What the
transcripts show is that there is nothing for a provider to refuse ownership of:
neither format carries an account identifier or a server-side conversation
handle, and both CLIs re-send the whole conversation each turn. That is an
inference from the transcript shape, not a measurement.

What a move does cost is the prompt cache, which is scoped to the credential:
the resumed turn measured here reported 34 596 input tokens against 26 368
cached, and on a move that cached share is billed in full. Failover should price
one cache-cold turn per move, and a run that ping-pongs between accounts pays it
every time — a reason to prefer the account whose window resets soonest and stay
on it.

Claude's transcript also embeds the originating account's e-mail address as
environment-context prompt text, so a session resumed under a second account
still names the first in its context. That is the harness writing its own
environment block, not YAD moving anything: it is prompt text, not a credential
and not an ownership claim, and nothing is copied out of an account home.

### What the providers document — not measured here

Read from vendor documentation, and separate from everything above, which was
run on this machine.

Anthropic's Messages API is documented as stateless: the client sends the whole
conversation on every request, so there is no server-side conversation for an
account to own. OpenAI's Responses API is the interesting one — it *does* offer
server-side state, through `previous_response_id`, stored responses and the
Conversations API, and state held that way would be scoped to the credential
that made it. Codex does not use it for resume: the resumed turn measured here
re-sent the whole conversation, which is why the move works.

So Codex's portability rests on a client-side choice its CLI could change in a
release, not on a property of the API. That is the concrete risk DEV-58 exists
to catch, and the reason the version pin at the top of this note matters.
