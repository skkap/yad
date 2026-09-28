---
status: amended by 0057 — a hub may add and remove accounts, unless the owner turns it off for its connection; Codex logs in from a hub by device code
date: 2026-09-25
---

# A hub may log an account in, by a link the owner follows or a token the owner pastes

Logging a work machine's harnesses in happens at the machine: a shell, a URL
opened elsewhere, a code or a token pasted back (0052, 0054). The hub is where
the owner already sees that a login is missing (0053), so it is where the owner
should be able to finish it. Decision 0054 left "relay Claude's interactive
login through the hub" waiting on Anthropic's terms, which forbid a service to
"intermediate" Claude.ai sign-in; the owner asked Anthropic support on
2026-09-25, and support said it is allowed.

What was measured, claude 2.1.281, on a work machine:

- `claude auth login` needs no terminal. With stdin and stdout as pipes it
  prints an authorize URL carrying a PKCE `code_challenge` (S256), waits at
  `Paste code here if prompted >`, reads the code from stdin and checks it.
  The code is useless without the verifier, which never leaves the process.
- `claude setup-token` prints nothing without a terminal, so it cannot be
  driven the same way.

## Decision

**Two ways, both started from a hub, both ending in yad's own login check.**

- **By link.** The hub sends `start_login` for a harness and an account. The
  runner runs the harness's own login in that account's home — for Claude,
  `claude auth login` over pipes — and reports the URL (for Codex's device
  code, a URL and a user code) in its next sync. The owner opens it, signs in
  with the provider, and pastes the code into the hub, which sends it with
  `login_code`; the runner writes it to the login's stdin. Only the URL is read
  from the harness's output; whether the login took is `account.LoggedIn`'s
  answer, never the harness's wording.
- **By token.** The owner runs `claude setup-token` wherever they like and
  pastes the token into the hub, which sends it once with `login_token`. The
  runner stores it as a token account (0054) and checks it. Unlike the code,
  the token is a working credential for a year: a hub holds it — sealed, where
  it has a way to seal secrets — only until the runner has answered the sync
  that carried it, which is the first sync reporting that login, and never
  shows it again. The runner never logs, echoes or reports it.

**Any connected hub may start a login.** The owner trusts the hubs it connects
([0038](0038-the-owner-trusts-the-hubs-it-connects.md)); a login is finished by
whoever holds the provider account, and a hub could already ask a harness for
anything the machine holds. What a hub may *not* do is decide the machine's
configuration: it logs in an account the owner listed in `config.toml`, or a
harness's own default login, and never creates an account.

**A login is short-lived state the runner reports.** Each carries a hub-chosen
id; the runner repeats its state — `starting`, `waiting`, `checking`,
`succeeded`, `failed`, `expired`, `cancelled` — in every sync until one carrying
the terminal state is answered, as closed sessions are (0035). One login at a
time per account; ten minutes to finish; `cancel_login` ends it. It is
advertised as the protocol feature `login`, and a hub sends none of these
controls to a runner that does not advertise it.

**A login lives in the runner's memory, and a restart forgets it.** Its
process cannot outlive the daemon, so nothing of it goes to `state.db`. Since
a runner reports every login until its end is answered, one it reported and
then leaves out of a sync while it was not over is one it lost, and a hub ends
it `failed`. Each control is repeated until the reports answer it, as every
control is: `start_login` and `login_token` until the login is reported,
`login_code` while it is reported `waiting`, `cancel_login` until it is
reported over.

## Considered options

**Owner opt-in per hub or per machine.** Weighed and not taken: it adds a gate
0038 already declined to add for every other thing a hub may ask of a machine.

**Drive `claude setup-token` on the machine through a pseudo-terminal**, so the
year-long token never leaves the machine. Needs a PTY — a new dependency or
`script(1)` — and the token way was asked for as a thing the owner pastes; it
remains the better shape for the token and is worth revisiting.

**The hub holds tokens and delivers them per run** (Terragon). Still what the
terms forbid, and every machine's subscription in one database — this decision
has the hub hold a token only until one delivery.

## As built

The runner reads one thing from the harness's login output — the first link
whose path is an OAuth authorize page — and gives it thirty seconds to
appear; ten minutes for the code after it; a minute to exit once it has the
code. A link login takes only when claude's own login exits 0 within that
minute and the check then says logged in: the exit is a condition, never its
wording, and the check alone would say yes to a credential already in the
home — an account's own login from before it ran on a token — so a mistyped
code would end succeeded and remove the token. A plain `yad account add`
counts the same way.

A link login's harness is given a browser that opens nothing (`BROWSER=true`;
[DEV-133](https://zumino.cc/app/yad/t/DEV-133)). Measured on claude 2.1.282:
over pipes it still opens the machine's browser before printing the link, and
a browser there signed in to claude.ai finishes the login through its own
callback — as whoever is signed in there, with no code. A hub login is signed
in by the person at the hub, so the only way through is the code. Should a
login finish before any code all the same and the account then be logged in,
the report says something on the machine signed it in and how to check which
account it is, rather than a bare failure. `yad account add`, with the owner at
the machine, still opens the browser. A link login on a token account leaves the token in its file for the
runs still on the account, runs and judges the login without it — claude's
check says yes to any token — and removes it only once the login has taken;
a plain `yad account add` does the same. A login holds its account as a run
does, so removing the account keeps the home until the login has stopped and
then deletes it with whatever the login wrote; the removal ends the login
`cancelled`.

`yad hub` keeps a token as it keeps a grant (0041): in `hub.db`, blanked by a
trigger the moment the runner reports the login and never returned by its
API. `requested` means only that no report has come, so the hub records when
an answer first carried a login and ends one on its own word only while none
has: a login never sent is ended at once by a cancel or a newer login, and
expires after ten minutes, token and all. Once one has gone out, the runner
may have stored its token or taken it, so a cancel or a newer login sends
`cancel_login` and waits for the runner's report. One unheard for half an hour
from its start — past every deadline a runner holds a login to — ends
`failed` and loses its token, so a runner that took the answer and never
synced again cannot leave a token held for ever; this is a MUST for any hub
(HUB.md §7). Should a hub-written end still meet a runner's report of
one, the runner's report wins: it knows what happened on the machine.
