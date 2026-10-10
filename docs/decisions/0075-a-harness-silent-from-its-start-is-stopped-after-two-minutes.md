---
date: 2026-10-11
---

# A harness silent from its start is stopped after two minutes, and a watchdog's error quotes its stderr

On a Mac runner (yad v0.1.0, the service in the desktop session) every Claude
run sat in `running` with no event at all until it was cancelled, three times,
three minutes or more each. `yad doctor` said Claude was ready. The cause was
outside yad: native Claude hung right after loading its settings when it ran
from the owner's shared `~/.claude`, where another `claude` ran in the
background. yad gave no sign of any of it. The only watchdog that could have
fired was the inactivity timeout, 30 minutes by default, and when it fires its
error says nothing of what the harness was doing. DEV-170.

## Decision

The maintainer was away. The agent working DEV-170 decided this, and the
reasoning is on the ticket and the pull request for the maintainer to review.

**A new watchdog, before the first event.** Until a turn's harness produces
its first event, the executor's timer runs to the owner's first-output timeout,
`[supervise] first_output`, 2 minutes by default. Once an event has arrived the
timer runs to the inactivity timeout as before. A run whose `inactivity_ms` is
lower than the first-output timeout is stopped at its own inactivity and ends
`inactivity_timeout`, as it asked. A hub cannot set the first-output timeout.
When it fires, the turn climbs the cancel ladder as every watchdog does, and
the run ends `timed_out` with the error class **`harness_silent`**. It counts
as a stall in `metrics.stalls`.

- **Events, not bytes.** The timer resets on what the adapter turns into an
  event, in the executor, which is the one place every adapter's turn passes
  through. So it covers Claude, Codex and OpenCode alike without touching an
  adapter. It runs again on each turn of a run that moves between accounts.
  Each adapter's first event comes as soon as its harness is talking: Claude's
  `system/init` frame becomes the `started` status, and Codex and an ACP agent
  emit `started` once their handshake has been answered. A harness that wrote
  lines the adapter skipped, or only stderr, has still told the run nothing.
- **Two minutes.** It has to be well past the slowest start that is working,
  and well before a person gives up and cancels. The slow starts known so far
  fit: Claude connecting its MCP servers or running a SessionStart hook before
  its init frame, and an ACP agent's cold start, whose handshake steps are
  already bounded at 60 s each. Codex's handshake steps are bounded at 30 s.
  The owner of the Mac cancelled after three minutes. Two minutes catches the
  wedge 28 minutes sooner than the inactivity timeout did, and a machine with a
  slower start can raise it in `config.toml`. "0s" or unset is the default, as
  for `inactivity`. A value at or above the inactivity timeout leaves that
  timeout alone to decide.
- **A new class rather than `inactivity_timeout`.** A hub acting on the class
  needs to tell the two apart. A run that went quiet half an hour into its work
  may be worth sending again. A harness that never said a word is a fault of
  the machine, and the same run sent back to the same runner is likely to end
  the same way. Error classes are an open set (`RunError.class` says so, and
  HUB.md says to show a class you do not know), so this is an addition to the
  protocol, not a change to it. The `class` and `inactivity_ms` descriptions in
  `openapi.yaml` name it.
- **The error carries the next action.** It says how long the harness was
  silent, then quotes the end of its stderr, then gives the harness check to
  run by hand in the run's home. That is the same `Spec.HarnessCheck` the
  adapter's own errors give, `claude -p hello` for Claude, carried to the
  executor in `Outcome.Check`. An adapter with no check of its own gets
  `yad doctor` instead. A run on the default login of a harness that can have
  accounts (Claude, Codex; not OpenCode, whose `account add` is refused) also
  gets `yad account add <harness> <label>`, because the one cause known is a home
  shared with something else running there, and an account is a home nobody
  else uses. README and `docs/install.md` now recommend an account over the
  default login on a shared machine for the same reason.

**A watchdog's error quotes the end of the harness's stderr, and so does
`harness_exited`, as up to five lines rather than one.** Decision 0064 let a
run's error carry a harness's last stderr line and said that child text was
"not widened". This widens it, within these bounds:

- `adapter.StderrTail` keeps the last 5 non-empty lines, at most 1 KiB, joined
  by ` | ` into one line. A hang's last line is usually a log line, and the
  lines before it say where the harness stopped. The adapters hand it to the
  executor in `Outcome.Stderr`, and the executor adds it to the error of any
  watchdog that stopped the turn. Before this, a watchdog's error never said
  anything about what the harness had been doing.
- Before any of that, **every value yad put in the harness's environment is
  replaced with `[redacted]`**. That means every value of 8 bytes or more in
  the environment the adapter started the harness with: the account's token
  (`CLAUDE_CODE_OAUTH_TOKEN`), its home, the run's grants and their file
  paths, and what an ACP agent's `Prepare` added, OpenCode's server password
  among them. A value of several lines, such as a PEM key delivered as a
  grant, also has each of its lines of 8 bytes or more replaced on its own, so
  a tail cut part-way through it carries none of its later lines. These are the credentials yad itself handed the
  child, and the likeliest to come back, since a harness whose login is refused
  may quote the token it was refused. Values shorter than 8 bytes are left,
  because blanking every "1" would wreck the text and hide no credential.
- When the supervisor's 2 KiB tail is full, its first line may begin part-way
  through, and a credential cut at its start would no longer match. That line
  is dropped, as `internal/workdir` already does for git's stderr.
- Nothing from the environment is ever added to the tail. It still goes only to
  the hub that sent the run (0064). It is still harness output, quoted as data
  and never acted on.

The owner's own environment, apart from the account variables Scrub already
removes (0060), is not searched for credentials. yad cannot tell which of its
values are secret, and a harness that printed its whole environment would be
the harness's bug. This change does not try to cover that.

**`yad doctor` over SSH on a Mac does not call Claude's default login
missing.** macOS keeps the login keychain locked in an SSH session, so
`claude auth status` answers "not logged in" there whatever the keychain
holds. When `SSH_CONNECTION` or `SSH_TTY` is set, doctor shows such a harness as
`unknown over ssh` and does not count it as needing login. It prints a note
that says why, says that a daemon started from that session cannot read the
keychain either, and gives the two ways to get a true answer: `yad doctor` on
the Mac itself, or `security unlock-keychain` first. Accounts are not affected,
because their state is the daemon's own check. `--json` and the capability
document are not changed.

**No readiness probe yet.** The plan asked doctor and the capability document
to catch a harness that cannot answer. Every check that would have caught this
hang means running the model, which spends a token and needs the network, and
the suite may do neither. A cheap proxy, such as another `claude` process
running from the same home, is not reliable enough to decide readiness. That
work is DEV-175.

## Considered options

**Lower the inactivity default.** Multica's grew from 5 minutes to 2 hours,
because force-stopping a healthy run throws away the work. A silent start has
no work to lose, and only a separate timer can tell the two apart.

**Count bytes on stdout or stderr instead of events.** A harness that writes
lines the adapter skips, or only stderr, has still not started the run. It
would also mean a hook in `supervise` for each adapter, where the executor
already sees every event.

**Report `harness_silent` as an event and leave the run running.** The run
would still sit until the inactivity timeout, which is what DEV-170 is about.
Nothing has happened yet that stopping would lose.

**Let a hub set the first-output timeout.** That would add a protocol field for
something the owner's machine decides. `inactivity_ms` already lets a hub
shorten a silent start when it needs to.
