---
status: amended by 0055 — a hub may relay a Claude login, and deliver a token once; Anthropic support allowed it on 2026-09-25 — and by 0057, which builds the Codex device-code login from a hub this record left as backlog
date: 2026-09-25
---

# A Claude account may be a stored token, and every account shares the machine's config

Settles two things [0039](0039-accounts-log-in-themselves-and-the-soonest-reset-goes-first.md)
left open — "a long-lived token per account … is not relied on until the
harnesses commit to one", and "a way to finish a login remotely … later work" —
now that work machines (0052) put a runner on every VM, each needing logins
nobody can do there with a browser.

What was found, 2026-09-25 (research and measurements on claude 2.1.281):

- **Claude has no device-code login** (anthropics/claude-code#22992, open).
  `claude auth login` on a machine with no browser means a URL opened
  elsewhere and a code pasted back, per machine.
- **Claude has committed to a token.** `claude setup-token` prints a one-year
  OAuth token for a Pro, Max, Team or Enterprise plan, given to Claude Code as
  `CLAUDE_CODE_OAUTH_TOKEN`; Anthropic's own docs recommend it for scripts and
  CI, and the Claude Code GitHub Action uses it. It has no refresh chain, so
  unlike a copied `.credentials.json` — whose single-use refresh token forks
  and logs one copy out (claude-code#21765, #24317) — it survives being the
  only credential on a machine nobody logs into.
- **Anthropic's terms forbid a service to "collect, store, or intermediate
  Claude.ai credentials"** (code.claude.com legal and compliance). The owner
  storing their own token on their own machine is not that; a hub carrying it
  would be.
- **`claude auth status` answers `loggedIn: true` for any token**, valid or
  not. A run on a refused token fails with `is_error`, `terminal_reason`
  `api_error` and `api_error_status` 401.

## Decision

**A Claude account may be a token account.** `yad account add claude <label>
--token -` reads the token from stdin — never argv, which every account on the
machine can read — and needs no terminal, so a provisioning script can do it.
yad keeps it in the account's home as `yad-oauth-token`, `0600`, written beside
and renamed over; one others can read is not used. It reaches the account's
runs and its login check as `CLAUDE_CODE_OAUTH_TOKEN`, appended after the home
variable (grants may not name it, 0040), and no printed command, log, event or
hub message ever carries it. This is yad's first stored credential for an
account; 0039's "YAD stores no tokens" holds for every other kind.

**A refused credential parks the account.** Since the login check cannot tell a
dead token from a good one, the Claude adapter reports `api_error_status` 401
as `Outcome.AuthRejected` — the harness's structure, not its wording — and the
runner parks the account in `needs_login` on it without asking. The class a
hub sees is unchanged (`harness_error`); nothing is added to the protocol.
Three things keep that parking honest:

- **The refusal is held against the token the turn was handed.** Each turn
  reads the token once (`account.TurnEnv`) and keeps a hash of it; a refusal
  parks the account only if that is still the stored token. An owner who
  stores a new token and then revokes the old one has turns still running on
  the old one.
- **The login probe leaves a token account alone.** It would hear "yes" for the
  refused token and put the account back in service to be refused again, every
  interval. A token account comes back when a new token is stored, which tells
  the daemon itself.
- **Every next action for a token account is `--token -`** — the hub's
  no-free-account error, the runner's log and `yad account list`. A plain
  `yad account add` on a token account removes the stored token first and says
  so: left in place it would outrank the new login in every run.

**One reading of the token file.** A file that is not a plain file, is empty,
or can be read by others is not used, and every reader agrees: the account
shows `token ignored` in `yad account list`, with why and what to do, not a
working token nor a refused one.

**A token's year is counted from when it was stored.** Neither the token nor
Claude says when it expires. From eleven months on, the harness report carries
a warning naming the account and the date, and `yad account list` shows it.

**Every account home shares the machine's config.** A run on an account used
to see only that home, so the machine's `CLAUDE.md`, settings and skills —
everything a work machine's spec puts in the runner's home — applied to runs on
the default login and to no account. Each time a home is prepared, yad links
Claude's `CLAUDE.md`, `settings.json`, `skills/`, `commands/` and `agents/`, or
Codex's `AGENTS.md` and `prompts/`, from the harness's default home, when the
account has none of its own. What an account has of its own stays; a link whose
source is gone is removed. Never a credential, and never a file the harness
keeps state in, which two homes must not share.

**Codex logs in by device code** with `yad account add codex <label> --device`
(`codex login --device-auth`), still the harness's own login. Codex's
app-server can also run a device-code login over its protocol, which would let
a hub's UI show the link and code while the token stays on the machine; that is
a protocol addition, and backlog.

## Considered options

**Copy a logged-in `.credentials.json` or `auth.json` onto each machine.** The
refresh chain forks and the copies log each other out; OpenAI's own CI guide
says not to share one `auth.json` across machines.

**The hub holds the tokens and delivers them per run**, as Terragon did. It is
what Anthropic's terms forbid, and it puts every machine's subscription in one
database.

**Relay Claude's interactive login through the hub.** No device-code flow
exists to relay, and passing a login code through a service is close enough to
"intermediating" that it waits for Anthropic to offer a flow for it.

**Link the whole default home into each account.** It would share the
credential and the harness's state files, which is exactly what accounts are
there to keep apart.
