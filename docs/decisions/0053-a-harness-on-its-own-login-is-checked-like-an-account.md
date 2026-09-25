---
date: 2026-09-25
---

# A harness on its own login is checked like an account, and Claude's JSON answer decides

A harness the owner gave no accounts runs on its own default home
(ARCHITECTURE.md §"No accounts is a state"), and until now nothing asked whether
that home held a login. The first work machine (0052) showed what that costs:
Claude Code installed and never logged in was reported **ready** by `yad doctor`
and in the capability document, so a hub routing on readiness would have sent it
every run, and every run would have failed. AGENTS.md's guardrail — a runner
never accepts a run it cannot drive — was being kept for the binary and broken
for the login.

**The default home's login is asked, as an account's is.** For each drivable
harness with no accounts configured, the capability probe runs the same check
`yad account` uses (`claude auth status`, `codex login status`) against the
harness's default home. A definite "no" is an `error` on the harness — it is
not drivable, no hub offers it a run, and the error names the harness's own
login command, run as the runner's user. A check that cannot answer is a
warning and changes nothing: an unanswered question is not a "no" (0039,
DEV-26). `yad doctor` shows the harness as `needs login` rather than `broken`.

**Once a minute at most.** The daemon re-probes every 15 seconds; the answer is
kept for a minute per binary, so a person who just logged in sees it within a
minute and an idle runner starts one short process a minute per harness.

**The command leaves the machine without a path.** The error travels to every
hub, so it names the harness by its name, not the path detection resolved, and
a runner whose environment moves the harness's home (`CLAUDE_CONFIG_DIR`,
`CODEX_HOME`) shows that variable with a placeholder value (DEV-67).

**Claude's JSON decides, whatever the exit status.** Measured on claude
2.1.281: a home with no login answers `{"loggedIn": false}` and exits 1. The
account check read the exit status first and called every such answer "could
not tell" — so an account whose login had expired was never parked in
`needs_login`, and kept being handed runs that failed. The JSON's `loggedIn` is
now the answer; a failing exit with no JSON, or with `true`, is still an error.

## Considered options

**Leave it to the run.** The first run fails and says why. But nothing parks a
harness with no account, so every run after it fails the same way.

**A protocol field for it** (`needs_login` on the harness report). The error
already makes the harness not drivable and says why; a field is for when a hub
needs to act on it differently from any other error, which none does yet.
