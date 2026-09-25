---
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
  the token is a working credential for a year: a hub holds it sealed, only
  until the runner has answered the sync that carried it, and never shows it
  again. The runner never logs, echoes or reports it.

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
