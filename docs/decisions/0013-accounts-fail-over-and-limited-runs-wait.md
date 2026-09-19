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
shared directory linked into every account home. The client half of that is
measured on both CLIs — a home that never created a session rebuilds the whole
continuation from the shared directory. Whether a provider then accepts it under
a second subscription is not. The note below draws the line.

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
in the first home, and the second — which never saw it written — was pointed at
the same directory and asked to resume it. Each bullet below says which home it
was run in, because only one login per harness existed and the two homes could
not both talk to the provider.

- **A home that never created the session finds it.** Both CLIs resolve
  `resume` before they check the credential, so the lookup is visible even from
  a home with no login: an id present in the shared directory reached the auth
  gate, an absent one gave `No conversation found with session ID` and
  `no rollout found for thread id`.
- **That home reconstructs the whole conversation.** Each harness was told a
  word in the first home. The second was then pointed at a local HTTP endpoint
  that records the request and refuses it, and asked to resume: Claude sent
  167 718 bytes and Codex 81 711, both containing the word, on the session's own
  native id. So the request a foreign home puts on the wire is the real
  continuation, built from the shared transcript and nothing else. The Claude
  run did this under a different credential from the one that wrote the
  transcript — an API key rather than the subscription login — which is as close
  to a second account as this machine could get.
- **A resumed turn round-trips, in the originating home.** Reading the
  transcript through a link into the shared directory, a resume against the real
  provider returned the word on the same native id and appended to the same
  file, so the directory stays the one copy rather than gaining a fork per
  reader. This ran under the credential that wrote the transcript: it shows the
  shared directory is a working read and write path, not that a foreign home
  gets a reply.
- **No other directory had to be shared.** Codex writes a
  `threads` index in `state_5.sqlite` in `CODEX_HOME`, but it is a cache: a home
  with an empty database scanned the shared `sessions/` and rebuilt its own row.
  Claude's `session-env/<id>/` was empty here and resume worked without it —
  though `CLAUDE_ENV_FILE` lets a hook populate that directory, so a harness home
  driven by session hooks may have per-session state outside `projects/`. YAD
  sets no such hook.
- **Resume by id ignores the working directory.** Measured on both: a session
  created in one workdir resolved from an unrelated one, so Claude scans all of
  `projects/` rather than the subdirectory its cwd encodes, and
  `codex exec resume` behaves the same. Codex's interactive picker does filter
  by cwd — that one is read from its `--all` flag, not run.

**Not measured: a provider's server accepting that request under a second
subscription.** Everything up to the wire is measured; what is left is the
response. No second login existed on the machine, and neither creating one
unattended nor copying a credential into a test home was acceptable. Tracked as
**DEV-58**, so the gap stays a task rather than a caveat in a merged document.
What the transcripts show is that there is no handle for a provider to check
ownership against: neither format carries a server-side conversation id, and
both CLIs re-send the whole conversation each turn. They are not free of account
identifiers — Claude's embeds the originating account's e-mail address, below —
but that is prompt text the provider would have to go looking for, not a key it
is handed. That a provider does not go looking is an inference from the
transcript shape, not a measurement.

What a move should cost is the prompt cache. The measured part is one number:
the resumed **Codex** turn reported 34 596 input tokens against 26 368 cached —
a cache hit on a same-account continuation. Nothing here measured a miss after a
move, because nothing here moved accounts.

That it would miss is read from Anthropic's prompt-caching documentation, which
says caches are isolated between organizations and, on the Claude API, per
workspace within one. That page describes the API's tenancy rather than two
consumer subscriptions, so the mapping is a reading, not a quote — but every
isolation boundary it names sits at or above the account, and none of them puts
two separately logged-in subscriptions on the same side. So failover should
price one cache-cold turn per move, and a run that ping-pongs between accounts
pays it every time — a reason to prefer the account whose window resets soonest
and stay on it. DEV-58 confirms the miss at the same time as the move.

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
Conversations API — state a second account would have to be entitled to, on
terms this note did not establish either way. Codex does not use it for resume:
the resumed Codex turn measured above re-sent the whole conversation, which is
why the move works at all.

So Codex's portability rests on a client-side choice its CLI could change in a
release, not on a property of the API. That is the concrete risk DEV-58 exists
to catch, and the reason the version pin at the top of this note matters.
