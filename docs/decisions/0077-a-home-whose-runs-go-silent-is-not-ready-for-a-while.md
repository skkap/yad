---
date: 2026-10-11
---

# A home whose runs go silent is not ready for a while, and another Claude in it is a warning

YAD-170 found a Mac runner whose every Claude run hung before its first word.
Decision 0075 made such a run stop after two minutes, ending `harness_silent`.
It left the readiness question open: `yad doctor`, the capability document and
health all still said Claude was ready, so a hub kept sending it work, and
each run failed the same way. Every check that would catch the hang runs the
model, which spends a token and needs the network, and no probe may do either
(YAD-175).

## Decision

The maintainer chose both directions on the ticket. The agent working YAD-175
decided the details below; the reasoning is on the ticket and the pull request
for the maintainer to review.

### The runs a home has had are the readiness signal

**Per home.** The count is kept for each account, and for a harness's own
login when it has no accounts (label `''`). The one cause known is a home
shared with another Claude running there, and an account is a home of its own.
Readiness was already decided per account, with the harness's `ready` derived
from the accounts. One silent account must not take a working one down with
it. `account.Soonest` skips a silent account as it skips a limited one, so runs
move to an account that works.

**Three in a row.** Each turn on the home moves the count:

- a turn whose harness produced any event clears it;
- a turn stopped `harness_silent` adds one;
- anything else leaves it, such as a cancel before the first word, or a run's
  own shorter inactivity, which ends `inactivity_timeout`.

One silent start can be a slow cold start that ran past two minutes once:
MCP servers connecting, a SessionStart hook. Two can be two runs started
together into one passing fault. Three in a row with nothing said in between
is past coincidence. YAD-170 saw three, and three costs at most three
two-minute failures before hubs are told. Two would trip on the coincidence.
Four or more would spend a fourth failed run of somebody's work for little
more certainty.

**Then a cool-down, never a verdict.** A home that is not ready is offered no
run, so it has no way to prove itself; only the clock can bring it back. It is
left alone for 15 minutes after the third silent run, and for twice as long
after each further one (30 minutes, 1 hour, 2 hours), capped at 4 hours. Then
it is offered again and the next run decides. If it speaks, the record is
cleared. If it is silent again, a longer cool-down starts. A fixed cool-down
would fail a hub's run every quarter of an hour for ever against a cause that
stays, such as a `claude --bg` left running, which is 96 failed runs a day. The
doubling brings that down to about six a day, and the cap means no home is
ever more than four hours from its next chance.

**Ways back sooner.**

- A daemon start ends every cool-down in progress and keeps the counts. It is
  the owner's word that the cause is fixed, and `yad daemon restart` is the
  next action every message gives. With the count kept, a home still wedged
  is not ready again after one more silent run, not three. A self-update
  re-executes and so ends a cool-down too. That costs at most one run.
- Removing an account forgets its record. A new account is a new home with
  nothing against it.

**State.** A new table, `harness_silence` (migration 0010), holds harness,
label, runs, until and updated_at. `until` is the authority, as
`limited_until` is for a limit: the home is offered again the moment it
passes, and nothing has to write that down. The daemon is its only writer.
The executor writes it after each turn, in a transaction, because two turns
can end at once. The CLI reads it read-only through `account.ReadAll`, as it
reads account states (0043). The prune that forgets a removed account's rows
forgets its record too, and leaves a default login's alone.

**Where it shows.**

- **Health.** `ready: false` when no home of the harness can take a run. The
  claim already reads that and leaves the offer for the hub (0039), rather
  than refusing it: a refusal is terminal, and nothing is wrong with the run.
  A run claimed just before the trip and reaching the executor after it is
  refused naming the silent accounts.
- **Protocol, additive.** Two optional fields carry `silent_until`:
  `AccountReport.silent_until`, in the document and in health, and
  `HarnessHealth.silent_until`, for a harness on its own login, which has no
  account report to carry it. Without them a hub would see `ready: false`
  above accounts that all read `free`, the self-contradiction `capAccounts`
  exists to avoid. The state enum is not grown: it is closed for v1 (0047),
  and silence says nothing about the login or its quota, only about the home.
  `make check-breaking` passes.
- **The capability document** gets a warning on the harness with the time and
  `yad daemon restart`. For a harness on its own login that can have
  accounts, the warning also gives `yad account add <harness> <label>`. It is
  a warning and not an `error`: an error makes the harness undrivable, and the
  runner would then refuse its runs outright.
- **`yad doctor`** shows the harness `not ready` with the same warning, and a
  last line that says why nothing is ready.
- **`yad account list` and `yad hub runners`** say "silent until".
- **The run's error.** The error of the run that trips the home says so, and
  gives the restart command.
- **Logs.** A log line goes to recent errors, with the details in its attrs.

### Another Claude in the home is a warning

**What is read.** Claude keeps records of its own live processes inside its
home. Measured on 2.1.296:

- `sessions/<pid>.json`, one for every live session in the home, interactive
  and SDK alike (both seen); Claude's code also names a `background` kind,
  which the warning reports as `claude --bg`, though no such record was seen;
- `daemon/roster.json`, the `claude --bg` supervisor's, which names its pid
  as `supervisorPid`.

The probe reads `pid`, `procStart` and `kind` from the first, and
`supervisorPid` from the second. Nothing else from either file is kept,
printed or sent. The `.key` files beside them are never opened, and neither
is another process's environment or argv, either of which may hold a token.
On macOS `ps -E` would show the environment, and reading it would mean reading
whatever credential it holds.

**How a pid is checked.** One `ps -A -o pid=,ppid=,comm=` gives the process
table, and only when a record named a pid. A second ps gives the start times
of the few that are alive: `ps -o pid=,lstart= -p …`, with `TZ=UTC LC_ALL=C`,
because Claude records `procStart` in UTC and in the C locale's words. A pid
counts when three things hold:

- it is alive;
- it started when its record says, or its command is named `claude`, so a
  record left by a crash whose pid has since been taken by something else is
  not counted;
- it does not descend from this process or from any process named `yad`, so
  yad's own runs are not counted, whichever profile started them.

**Which homes.** The homes a run uses: the default home (`CLAUDE_CONFIG_DIR`,
or `~/.claude`) for a Claude with no accounts, and each account's home
otherwise. In the capability document the warning gives a count, with no path
and no pid. In doctor it also lists the pids. The daemon keeps what it found
for a minute, because it re-probes every 15 seconds.

**A warning and nothing more.** This is a proxy for a cause seen on one
machine, so it never makes a harness not ready. The runs a home has actually
had are what decide that. When there are no records, or no `ps`, or a `ps`
that fails, nothing is said, because absence is data. Doctor adds a note when
`ps` could not be read.

**Limits.**

- The check depends on Claude's own files, which are undocumented. A Claude
  version that keeps none is a home where nothing is found.
- A Claude that does not register a session is not seen: a running
  `claude setup-token` did not.
- On Linux `procStart` is not measured, so there the command name decides.
- An npm-installed `--bg` supervisor runs as `node`, and its roster has no
  start time, so it is missed.

## Considered options

**A Claude subcommand that loads settings as a run does and exits.** None was
found that does both without calling the API. `claude auth status` loads too
little: it answered in the YAD-170 home.

**Readiness per harness.** This is simpler, but one silent account would stop
every account of the harness, and an account is the fix 0075 recommends.

**A new account state, `silent`.** The set is closed for v1. A hub generated
from the document validates it strictly, and growing it would mean a feature
gate for a state that says nothing about the account.

**Silence as an `error` in the capability document**, as 0053 does for a
missing login. An error makes the harness undrivable, so offers for it are
refused, which is terminal for the run. It is also not a cool-down: nothing
would ever offer the home again for it to prove itself.

**A fixed cool-down, or none, until a person acts.** Without a cool-down a
runner stays stranded until someone notices. A fixed one keeps failing runs
at its own rate against a cause that stays.

**Clear the record on restart.** This would let a service manager's restart
loop, or a self-update, give a wedged home three more silent runs each time.
Ending the cool-down and keeping the count costs at most one run.

**Read the other processes' environments** to compare `CLAUDE_CONFIG_DIR`.
On macOS this works for the owner's own processes. It reads every variable
they hold, tokens among them, and that is the one thing this repository never
does.
