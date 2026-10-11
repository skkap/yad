---
date: 2026-10-12
---

# A hub may release a session to resume on another runner, unless the owner says no

Until now a session lived on one runner and was resumable only there
([0007](0007-sessions-map-never-wrap.md), DOMAIN.md): the harness's
conversation and the workdir are on that machine's disk. Zumino's outsourcing
keeps a manager session open across days of wake-ups, and JapanLaw #1
(ZUM-100) found what that costs — a runner that drains, dies or is replaced
strands the session, and every move means starting the conversation over
(DEV-43). Anthropic's own runners can hand an idle session back to be picked
up elsewhere.

## Decision

The owner's, on 2026-10-11:

- **Measure first, then build it for Claude alone.** Phase 0 measured a
  Claude conversation copied to another home and resumed from another
  workdir (below); it works, so phase 1 builds release end to end for Claude.
  Codex waits on DEV-58, the second-subscription question 0013 left open;
  OpenCode until its export and import are measured.
- **On by default; the owner can turn it off per connection.** A connection's
  `release_sessions = false` in `config.toml` does it, as `manage_accounts`
  does for accounts ([0057](0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md)).
  The reasoning is 0038's: the owner trusts the hubs it connects. What a
  release exposes is said plainly in `docs/run-it-safely.md`: the session's
  conversation — the owner's CLAUDE.md as the harness read it, the account's
  e-mail where the transcript names it, every tool output — goes to the hub
  and to whichever runner takes the session next.
- **Work the remote does not have is refused, unless the hub says
  `discard_local`.** The next runner builds the workdir again from the
  session's sources, so changes not committed and commits not pushed would be
  gone without a word. The refusal names what is there; a hub sends a run
  that commits and pushes first. yad never pushes on its own.

The agent working DEV-43 decided the rest:

- **Words.** The verb is *release* — DOMAIN.md's open question, settled. What
  travels is the **session archive**: never "bundle" or "transcript" in the
  protocol.
- **Protocol, all additive and gated.** `release` is a per-run feature
  ([0069](0069-a-per-run-feature-is-its-harnesss.md)): in each harness's
  list for an adapter that is an `adapter.Releaser`, runner-wide only while
  every drivable harness has it, and taken out of every list a hub is sent
  when its connection says `release_sessions = false`. A new control,
  `release_session {session_id, discard_local}`, repeated until the runner
  reports it. A new sync list, `released_sessions`, each entry carrying the
  archive's sha256 and size or an error with a code, message and next action
  — not a close reason, since those are closed for v1
  ([0047](0047-four-v1-protocol-rules-settled-by-the-clean-room-check.md)).
  `session.archive {sha256, bytes}` on the run that opens a released
  session, never beside `fork_from`. `archive_max_bytes` in the register
  answer and every sync answer, so a runner registered earlier hears it.
- **Transport.** `PUT /sessions/{session}/archive`, from the runner the
  session is bound to while a release is asked for, idempotent by its sha256
  header; `GET` the same path, answered only to a runner a run of the session
  is offered to or bound to. Binary bodies, outside the JSON calls: an
  archive can be a hundred megabytes, and base64 in JSON would add a third.
  The hub stores the bytes and reads none of them. A PUT's answer is an
  object, so a pre-signed URL can be added to it later.
- **The archive.** A gzip'd tar of `manifest.json` — harness, harness
  version, native id, the directory the conversation was in, and each git
  source's branch — and the conversation's files under `conversation/`.
  Standard library only. The runner unpacking one takes plain files and
  directories inside `conversation/` and nothing else, within size and count
  bounds.
- **The same session id.** A release unbinds the session on the hub. Its
  next run goes out `new: true` with the archive, to any runner advertising
  release for its harness **but any it was ever released from**: each of
  those closed the session and keeps the closed row, so it would refuse the
  run as a closed session. The runner that takes it imports the conversation
  under its native id. One its harness already has is refused when an open
  session there holds it, and replaced otherwise: it is this session's own,
  left by an import cut short or by an earlier stay. A run naming sources
  other than the ones the archive carries is refused, since that runner has
  only the archive's word for what the session was built from.
- **The releasing runner.** A release waits while a run is held in the
  session, as a close does, and claims no run in the session while it is
  under way. The archive is a 0600 file under `<data>/releases/`, built once
  and uploaded with the result outbox's patience — retried with backoff,
  again after a restart — and the session closes as `released` only once the
  hub answers 2xx. The collector then reclaims the workdir and WT_SLOTs, and
  the conversation's files are deleted. A path source is never released.
  Claude's export is `<native>.jsonl` and its companion `<native>/`
  directory, never `file-history`, `tasks` or `session-env`.
- **The runner taking it.** While preparing: download, check the size and
  sha256, unpack, import into the shared transcripts when the harness has
  accounts and the default home when not, under the project directory Claude
  names after this workdir; record the native id; then prepare the sources,
  a git source naming no branch checked out on the branch the archive says.
  A failure is the run's, class `archive_rejected` for an archive it will not
  take (error classes are an open set) and `prepare_failed` for a fault here.
  The session keeps the archive it is to open from, and its next run tries
  again rather than start the conversation empty.
- **yad hub** stores archives in `hub.db` and deletes one once a run of the
  session is reported running, or succeeds, on the runner that opened it, once
  the session closes (a trigger), or after `--archive-ttl` (seven days), when
  a released session nothing opened closes. A service API call
  `POST /sessions/{id}/release` and `yad hub release-session` ask for one;
  `GET /sessions/{id}` shows `release: releasing | released`, beside the
  state rather than in it, since the service API's enums are closed too.
- **HUB.md** tells a hub to send an archive only to a runner in the same
  trust domain as the one that released it.

## Measured: phase 0, claude 2.1.296, 2026-10-12

On the owner's Mac, with the owner's approval, in scratch homes: each run had
its own `CLAUDE_CONFIG_DIR` and `HOME`, the owner's login handed in as
`CLAUDE_CODE_OAUTH_TOKEN` for the one process and written nowhere — no
Keychain item and no `.credentials.json` appeared in any scratch home. Model
haiku; the three turns cost $0.0124.

- **A**: home A, workdir `runnerA/workdirs/conn/sess-1`, `--session-id`. The
  turn was told a codeword and ran `seq 1 60000`, whose 340.7 KB output Claude
  saved to `<home A>/projects/<A's project directory>/<id>/tool-results/` and
  named by that absolute path, four times in a 25-line, 79 845-byte
  transcript; the companion directory was 344 KB. The transcript holds a
  signed thinking block and A's workdir as `cwd` on 18 lines.
- **B**: `<id>.jsonl` and `<id>/` copied to home B under the project
  directory of a different workdir, `runnerB/data/workdirs/other-conn/
  moved-session`, and home A moved away. `--resume <id>` succeeded under the
  same session id and answered the codeword; Claude appended to the copy in
  place, 25 lines to 40. The saved tool output's path, into A's home, was
  gone: "No such file or directory", which the model said and carried on
  past. It noticed its working directory had changed.
- **C**: the same copy, with the companion directory's path rewritten to C's
  inside the transcript: the resume answered the codeword and read the saved
  tool output back from its new path.

So resume from a copied home and another workdir works, and the import
rewrites the companion directory's path — recognised by its end, the project
directory and the session id, since Claude names it through whichever home a
run used — everywhere but inside a thinking block, whose signature covers its
text. The codeword-from-a-copy measurement extends 0013's, which found a home
that never wrote a transcript resolving it by id; it is pinned to 2.1.296 for
the same reason.

## Not measured

- **A second subscription.** Both homes ran on the owner's one login. Whether
  the provider takes a conversation started on one subscription under another
  is DEV-58's, as it is for account failover (0013); a transcript carries
  signed thinking blocks, which makes that question sharper for a release.
- **Version skew.** The manifest records the releasing runner's Claude
  version; nothing refuses a different one on the next runner yet.

## Considered options

**A new close reason, `released`** — the set is closed for v1, and a hub
built before it would refuse the sync carrying it and renew no lease.
**Carrying unpushed work in the archive** — a second, larger transport for
something git already moves, and a workdir that differs from its remote in
ways no hub can see. **Unbinding without an archive and letting the harness
start over** — the work the session existed for is the conversation.
**Rewriting nothing** — measured: the saved tool output is gone on the next
runner, though the model works around it.

## Consequences

DOMAIN.md's session rule becomes "resumable only on the runner that holds
it, until its hub releases it". A hub that never asks for a release sees
nothing new but a feature string and an optional field.
