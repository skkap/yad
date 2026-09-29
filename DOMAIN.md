# Domain

YAD runs coding-agent harnesses on machines you own, for whatever hub asks — a
task tracker, a personal assistant, anything that speaks the runner protocol.

The vocabulary. A word defined here means this and nothing else, in the code, in
the protocol, in the CLI and in conversation. Where this file and a comment
disagree, this file wins; where it and `ARCHITECTURE.md` disagree on a *word*,
this file wins, and on *shape*, that one does.

`_Avoid_` binds working language — code, comments, prompts, commits, the
protocol. It binds published copy only where an entry says so.

## Language

### The machine

**Runner** — one `yad` process with one identity. The unit a hub routes to.
Identified by a random id written once to its profile's `runner-id`, never by
hostname: hostnames are duplicated across cloned VMs and reassigned by DHCP, and
two machines claiming to be `ubuntu-01` breaks session affinity silently.
Deleting the id file retires the runner and creates a new one — that is the
supported way to decommission a machine.
_Avoid_: agent (Buildkite's word for this), worker, daemon (the process mode, not the thing), node
_Rules_: A runner is the unit of trust. Work that must not meet — personal and
employer, two customers — runs on two runners, ideally as two OS users.
_See_: [0003](docs/decisions/0003-hub-is-a-role-runner-is-multi-homed.md), `internal/runner`

**Profile** — one runner's config directory: its identity, connections,
accounts and state. One machine can carry several profiles, and each is a
separate runner that knows nothing of the others.
_Avoid_: using it for per-run harness settings — a run's model and effort travel with the run, and nothing else does
_See_: `internal/config`

**Work machine** — a Linux VM built by `machines/yad-machine` to hold one
runner and nothing of the host it runs on: no mounted directory, no forwarded
port, and a runner's user that reaches the internet but not the host, its LAN
or its tailnet. The shape of "one runner per trust domain" that holds on a Mac,
where a second OS user cannot keep a runner service up.
_Avoid_: area, box, environment, sandbox — it confines what the machine reaches,
not what a hub may ask of it
_See_: [0052](docs/decisions/0052-a-work-machine-is-a-lima-vm-built-from-a-spec.md), `machines/README.md`

**Machine spec** — the directory a work machine is built from: `machine.env`
(name, size, harnesses, egress), the runner's `config.toml` — every setting
but the connections, and accounts it makes sure of and never removes
([0059](docs/decisions/0059-up-brings-a-machines-config-onto-its-spec-and-never-removes-an-account.md)) — an optional
`provision.sh`, and `home/`, laid over the runner's user's home. It holds no
secret; the machine's own credentials are logged in by hand, once.
_Avoid_: spec alone where a hub's run spec could be meant

**Hub** — whatever gives a runner work: anything that hosts the server half of
the runner protocol. A role, not a product — Zumino becomes a hub by embedding
it, yashiki by embedding it or by using `yad hub`, which is the standalone one.
_Avoid_: control plane, master, server, upstream, provider, dispatcher
_Rules_: The owner trusts the hubs it connects: a hub writes the brief, and the
brief can ask the harness for anything the machine allows, so YAD does not police
what a hub sends ([0038](docs/decisions/0038-the-owner-trusts-the-hubs-it-connects.md)).
What the owner configures (permission mode, sandbox, caps) no protocol field
can set. Accounts are the one exception, by the owner's choice: a hub may add and
remove them unless the owner turned that off for its connection
([0057](docs/decisions/0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md)).
_See_: [0003](docs/decisions/0003-hub-is-a-role-runner-is-multi-homed.md), `internal/hub`

**Connection** — one runner's standing registration with one hub: the hub URL,
the runner credential, and the owner's caps for it. A runner has one connection
per hub and any number of hubs.
_Rules_: `yad disconnect` ends one in a fixed order: the hub retires the runner
first — its held runs lost, its sessions there closed — then `config.toml` and
the credential lose it, then a running daemon lets it go. What a connection
`config.toml` no longer lists leaves on the runner — sessions, parked runs,
runs a crash left, reports owed — is ended there: at once when the daemon is
told, at its loop's next refused sync when it is not, and at the next start
when no daemon ran. Only the hub's own answer makes a credential dead; one
that cannot be read here never is.
_See_: [0072](docs/decisions/0072-disconnect-retires-at-the-hub-then-tells-the-daemon.md), `internal/config`, `internal/runner/disconnect.go`

**Registration token** — the one-time, short-lived secret a hub issues so that
`yad connect` can register a runner. Exchanged once for a **runner credential**,
which is per runner, revocable and rotatable, and is the only secret kept.
_Rules_: A registration token is dead after its exchange; the runner never stores it.
_See_: [0010](docs/decisions/0010-registration-token-exchanged-for-runner-credential.md), [0020](docs/decisions/0020-the-registration-token-may-be-typed.md)

**Admin token** — the secret a service or a person presents to `yad hub`'s
service API to submit runs and follow them. Created on the hub's machine, named,
revocable, stored by the hub only as a hash. A third kind beside the
registration token and the runner credential, and never accepted where either
of them is: a runner cannot submit work, and a service cannot pose as a runner.
Only `yad hub` has one — a hub that embeds the protocol makes runs its own way.
_Avoid_: API key, PAT — Zumino's word for a person's token
_See_: [0022](docs/decisions/0022-hub-service-api-beside-the-protocol.md), `internal/hub/admin.go`

**Capability document** — what a runner advertises to each hub: its id, name,
OS, arch, labels, `yad` version, the harness list with versions and accounts,
host tools, capacity, and the protocol features it supports. Public surface —
every field is one somebody will depend on.
_Rules_: Every connected hub reads it, so the runner writes it without a path
on the machine and without quoting what a harness or tool printed (DEV-67). A
run's error goes only to the hub that sent the run, and may name a path
([0064](docs/decisions/0064-a-runs-error-may-name-a-path-on-the-runner.md)).
A **per-run feature** — steer, interrupt, effort, fork — is the harness's:
each harness lists its own, and the runner-wide string says only what every
harness it drives supports
([0069](docs/decisions/0069-a-per-run-feature-is-its-harnesss.md)).
_Avoid_: capabilities for anything else — yashiki's "capabilities" are its house
tools, which here are **host tools**
_See_: `internal/capability`, `protocol/v1`

**Fingerprint** — a hash of the capability document with the timestamp removed.
Syncs carry the fingerprint; a hub asks for the full document only when it moves.
_Avoid_: using it for detection (Nomad's "fingerprinting") — here it is only the hash

**Labels** — free-form routing strings the owner attaches to a runner
(`linux`, `gpu`, `work`). Hubs route on them; YAD never interprets them.

**Host tools** — the non-harness executables a run may need from the machine
itself: `git`, `gh`, `docker`, and only those three. Probed and advertised so a
hub can route "needs gh + docker" to a runner that has them — and reported
whether or not they are there, since a missing docker and a gh nobody has
signed in are both answers a hub routes on. Anything else a run needs, it
installs itself.
_Rules_: Long-lived credentials behind host tools belong to the machine and never
travel in the protocol.

**Present** — the runner found a binary for a harness or a host tool: the file
its `YAD_<ID>_PATH` override names, or the one on `PATH`. For a harness it is
the binary every run starts. For a host tool it is the binary that was probed,
and the one a run uses: yad's own git, and the harness and setup hook finding
the tool by name ([0045](docs/decisions/0045-runs-use-the-host-tools-detection-resolved.md)). It
does not mean the binary works. A present harness with an `error` takes no
runs.
_Rules_: One rule for harnesses and host tools. An override **names nothing**
when nothing is at the path or it is a directory. Then `PATH` decides and the
override is reported: as a warning when `PATH` has the binary, which is still
usable, and as an error beside `present: false` when it has none. A file that
is there but will not run is present, with an error.
_Avoid_: installed — it has meant present, and it has meant working
_See_: [0044](docs/decisions/0044-an-override-that-names-nothing-lets-path-decide.md), [0045](docs/decisions/0045-runs-use-the-host-tools-detection-resolved.md), `internal/probe`

**Capacity** — how many runs a runner executes at once. One shared pool, with an
optional cap per connection and per harness, all set by the owner.
_Avoid_: slots (that word is `WT_SLOT`'s), concurrency, max tasks
_See_: [0005](docs/decisions/0005-pull-by-periodic-sync.md)

### The harness

**Harness** — a coding-agent CLI installed on the runner: `claude`, `codex`. Not
a persona, not an assistant, not a Zumino participant.
_Avoid_: agent — it means a PAT holder in Zumino, a resident's folder in
yashiki and a persona in Multica; provider; executor; engine
_See_: `internal/harness`

**First-class harness** — one YAD has an adapter for: it knows the streaming
format, the session-resume mechanism and how to interrupt a turn. Only a
first-class harness may be the target of a run. Everything else in the catalog is
**recognised** — detected and reported so the gap is visible, and refused as a
target.
_Kinds_: first-class | recognised
_See_: `internal/harness/catalog.go`

**Adapter** — the code that drives one harness: spawn, translate its stream into
events, resume, steer, interrupt, read usage. One package per first-class
harness.
_Avoid_: driver, provider, backend, runtime
_See_: [0006](docs/decisions/0006-claude-by-stream-json-codex-by-app-server.md), `internal/adapter`

**Account** — one harness login (subscription) on a runner, with its own harness
home (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`), logged in by the harness's own login —
or, for Claude, a **token account**: a `claude setup-token` token the owner
pipes in, which yad keeps in the home and hands to the account's runs. Every
home shares the machine's own instructions, settings and skills from the
harness's default home. Added by the owner at the machine, or by a hub the
owner allows to (a **hub login** with `add`); either way it is the machine's,
and every connected hub's runs rotate through it — the free account whose
window resets soonest takes the next run. Hubs see an account's label, state
and reset time, never its credentials.
_Rules_: An account is listed only once its harness's own login check says yes;
a login that does not take adds nothing. A harness with no accounts runs on its
own default login, and the first account added takes that login's place. A
run's credential is its account's and nothing else: a variable that would
choose another — an API key, a profile, a base URL — refuses the run when a
hub sends it as a grant, and is removed from every run when the owner's
environment holds it; a harness is asked whether an account can take a run in
the environment its runs get.
_Kinds_: free | limited | needs_login
_See_: [0013](docs/decisions/0013-accounts-fail-over-and-limited-runs-wait.md),
[0039](docs/decisions/0039-accounts-log-in-themselves-and-the-soonest-reset-goes-first.md),
[0043](docs/decisions/0043-the-cli-never-writes-state-and-account-changes-reach-the-daemon-live.md),
[0054](docs/decisions/0054-a-claude-account-may-be-a-token-and-every-account-shares-the-machines-config.md),
[0057](docs/decisions/0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md),
[0060](docs/decisions/0060-the-owners-own-account-variables-are-removed-from-every-run.md)

**Hub login** — an account's login started from a hub: by **link**, where the
runner runs the harness's own login and the hub shows its URL — and takes the
code the owner got back, for Claude, or shows the user code the owner types at
the URL, for Codex's device code — or by **token**, where the owner pastes a
`claude setup-token` token into the hub and the runner stores it as a token
account. Either way the credential lives on the machine and yad's own login
check decides whether it took. For an account already listed, a harness's own
default login, or — with `add`, from a hub the owner allows — a new account,
listed only once the login takes.
_Avoid_: remote login (it says nothing about who drives it), OAuth (one of the
ways, not the thing)
_See_: [0055](docs/decisions/0055-a-hub-may-log-an-account-in-by-link-or-by-token.md), [0057](docs/decisions/0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md)

**Usage limit** — a subscription window an account has exhausted: Claude's
five-hour and weekly limits, Codex's primary and secondary windows. Has a reset
time.
_Avoid_: rate limit for this. A **rate limit** is transient API throttling the
harness retries by itself (`system/api_retry`); it never makes a run wait.

**Model** — what the harness is pointed at for one run (`opus`, `gpt-5.1-codex`).
Chosen per run, never per runner: the same machine serves cheap and expensive
work.

**Effort** — how hard the harness thinks on one run, in the harness's own
words: Claude Code's `--effort` (`low` … `max`), Codex's reasoning effort
(`low` … `xhigh`, more for some models). Optional; absent is the harness's
default. Chosen per run, like the model, and never checked by the runner — the
harness decides which levels exist.
_See_: [0049](docs/decisions/0049-a-runs-effort-is-the-harnesss-word.md)

### The work

**Session** — a durable conversation with one harness in one workdir. Has a YAD
id and, underneath it, the harness's own native session id (Claude's
`--session-id`, Codex's thread id). The mapping is the entire reason sessions are
a first-class thing here: a hub must be able to say "continue the conversation
you were having about ZUM-19" without knowing what a rollout file is.
_Kinds_: per_run (a fresh harness process per run — the default) | live (one
process kept across runs — reserved, not built)
_Rules_: A session lives on one runner and is resumable only there. At most one
run is live in a session at a time. A session may move between accounts of its
harness: the transcript is the whole of the harness's side of it, it lives once
in a directory every account home links to, and a home that never created the
session rebuilds the conversation from it alone — measured on both CLIs rather
than assumed.
[0013](docs/decisions/0013-accounts-fail-over-and-limited-runs-wait.md) carries
what was measured and what was not.
A session **closes** — by its hub's word, its owner's, the idle TTL
or disk pressure — only while no run is held in it, takes no new run after, and
its hub is told why ([0035](docs/decisions/0035-a-runner-reports-every-close-in-its-sync.md)).
When its runner deregisters, the hub closes it on its own and ends the runs
still queued in it: the session is never handed to another runner, since it is
resumable only on the one that went. The runner closes it too, as closed by
the owner, and tells no hub.
A session may be opened as a **fork** of another on the same runner: its
conversation starts as the harness's copy of that one's, and the two diverge —
the one forked goes on untouched. A fork is a session like any other from its
first run on, with its own workdir.
_States_: open | closed (its hub or its owner closed it) | expired (the idle TTL
or disk pressure did)
_Avoid_: thread, conversation, chat — as names for this; Codex's "thread" is the
native id underneath
_See_: [0007](docs/decisions/0007-sessions-map-never-wrap.md), [0031](docs/decisions/0031-a-failed-resume-is-the-hubs-to-decide.md), [0065](docs/decisions/0065-a-fork-is-a-new-session-opened-from-another-sessions-conversation.md), `internal/store`

**Run** — one turn executed against one session, by one harness, on one model:
one prompt in, one terminal state out. The unit of work claimed, streamed and
reported. It may take seconds or hours. A session has many runs; a run belongs
to exactly one session.
_Avoid_: task, job, thread, turn — task and job are the hubs' words (Zumino's
task, yashiki's job) for the thing that *produces* runs; turn is the harness's
word for what a run executes
_See_: `internal/runner`, `protocol/v1`

**Start time** — an optional moment a run must not start before. One-shot, like
an email API's `send_at`; the hub may hand the run over early so it starts on
time.
_Rules_: No recurrence exists anywhere in YAD. A schedule is a hub's, and it
makes runs.
_See_: [0008](docs/decisions/0008-runners-hold-no-schedules.md)

**Brief** — what a run is told. Two parts: the **context**, the run's
standing background, kept outside the conversation — Claude's system prompt,
Codex's developer instructions — so it survives compaction, and reaching the
harness on every run of a session, not only the first; and the
**instruction**, which is the run's one user turn.
_Avoid_: prompt, for the whole thing — the prompt is only the instruction

**Sources** — the material a run's workdir is built from: git repositories
(URL, base ref, branch) or an existing local path. Optional — a run that answers
a question or uses host tools has none.
_Avoid_: folder source — say path source, or source on the machine for both kinds
_Rules_: A source is hub input. It reaches the network over https or ssh with
the machine's own credentials — or with the one an https URL's userinfo
carries, which is the run's alone, like a grant: taken out of everything
stored and given to git only in its fetch's environment — and the machine
itself only inside the roots the
owner allows — their home directory when they have listed none — and not at all
when the owner has set `path_sources = false`. A **source on the machine** is a
path source, or a git source whose URL is a local path or `file://`: the two
reach the same directories, and the roots and the switch govern both. A session keeps
the sources its workdir was built from; a run continuing it names the same ones
or none.
_See_: [0033](docs/decisions/0033-sources-reach-only-what-the-owner-allows.md), [0038](docs/decisions/0038-the-owner-trusts-the-hubs-it-connects.md), [0062](docs/decisions/0062-an-owner-may-switch-sources-on-the-machine-off.md), [0068](docs/decisions/0068-a-credential-in-a-source-url-is-the-runs-alone.md), `internal/workdir`

**Workdir** — the directory a session's runs execute in. Owned by the session,
kept between its runs, reclaimed after it closes. Built from the sources, or
empty when there are none.
_Avoid_: workspace — the tenant in Zumino and in Multica
_See_: [0011](docs/decisions/0011-hub-closes-sessions-runner-collects.md), [0032](docs/decisions/0032-a-workdir-belongs-to-its-session.md), [0035](docs/decisions/0035-a-runner-reports-every-close-in-its-sync.md), `internal/workdir`, `internal/runner/collect.go`

**Setup hook** — a repository's own `.worktree/setup`, which YAD runs in a new
workdir with the `WT_*` variables. Optional; a repo
without one still runs, and the run's events say so.
_Rules_: A hook that fails fails the run while it is preparing; the session's
next run tries it again.
_See_: [docs/setup-hooks.md](docs/setup-hooks.md), [0034](docs/decisions/0034-a-failing-setup-hook-fails-the-run.md)

**Slot** — `WT_SLOT`: a small integer unique among one repository's live
worktrees on a machine, from which the setup hook derives ports and container
names.
_Avoid_: slot for a unit of capacity

**Grant** — a short-lived secret a hub attaches to one run, scoped to it — a
Zumino token limited to one task. Delivered in the environment or a `0600` file,
never argv, and destroyed when the run ends — on the runner, and in the hub's
store, which keeps its name and blanks its value once the run is terminal. A
grant is for the work, never for the harness's own login: it may not name a
variable that chooses whose credential a harness uses or which home it logs in
from (`ANTHROPIC_API_KEY`, `CLAUDE_CONFIG_DIR`, `CODEX_HOME` and the rest of
`protocol/v1/grant.go`'s list), because that would move the run off its
**account**.
_See_: [0009](docs/decisions/0009-machine-owns-credentials-hubs-grant-per-run.md),
[0038](docs/decisions/0038-the-owner-trusts-the-hubs-it-connects.md),
[0040](docs/decisions/0040-a-grant-may-not-move-a-run-off-its-account.md),
[0041](docs/decisions/0041-a-hub-holds-a-grant-only-while-its-run-can-use-it.md)

### The lifecycle

**Sync** — the runner's periodic request to one hub. One call is the heartbeat,
the lease renewal for every run it holds, the health report and the ask for
work; the answer carries new runs (never more than free capacity), control
messages and the interval until the next sync.
_Avoid_: poll and heartbeat as separate things — there is one call
_See_: [0005](docs/decisions/0005-pull-by-periodic-sync.md), [0063](docs/decisions/0063-a-hub-holding-work-for-a-runner-asks-it-back-in-3-s.md), [0066](docs/decisions/0066-a-run-ending-brings-its-connections-next-sync-forward.md)

**Claim** — a runner taking a run offered in a sync. **Lease** — the hub's
promise that an offered or claimed run is this runner's for now; each sync
renews a claimed run's, and a lease that lapses puts an unclaimed offer back in
the queue and makes a claimed run **lost** from the hub's side — or
**cancelled**, for a claim the hub had asked to cancel, which the runner may
have withdrawn unstarted
([0061](docs/decisions/0061-a-cancelled-claim-the-runner-withdraws-ends-cancelled.md)).
**Abandon-after** — how long a hub lets a runner go without a sync before it
closes that runner's sessions; the runner keeps its credential.
_See_: [0046](docs/decisions/0046-a-silent-runner-loses-its-offers-with-the-lease-and-its-sessions-after-a-day.md)

**Event** — one normalised thing that happened in a run, numbered by a per-run
sequence and streamed to the hub in batches. The same set for every harness; a
hub never parses a harness's own format.
_Kinds_: text | thinking | tool_call | tool_result | status | usage | error
_See_: [0012](docs/decisions/0012-events-are-normalised-and-live.md), `protocol/v1`

**Run state** — where a run is.
_Kinds_: claimed | preparing | running | waiting | succeeded | failed | cancelled | timed_out | lost
_Rules_: The last five are **terminal**; a run reaches exactly one and never
leaves it. **Waiting** holds no process — the run is parked on a usage limit
with a resume time, and survives a runner restart.
_See_: `protocol/v1/run.go`

**Interrupt** — end the current turn and keep the session: Claude's `interrupt`
control request, Codex's `turn/interrupt`. **Cancel** — end the run, escalating
from interrupt to the process group's signals. **Steer** — add input to a turn
already running.

**Drain** — stop claiming, let live runs finish, then exit. What an upgrade, a
reboot or a retiring machine does. A draining runner keeps syncing, so leases
renew and results land, and says `draining` in its health.
_Rules_: Stop signals are counted: the first drains, the second cancels the runs
held, the third exits at once. A drain lets runs finish for the owner's drain
wait, then cancels them — all but a self-update's, which has no wait, and does
not say `draining` either, since the runner comes back. The hub's `drain`
control is the first step.
_See_: [0029](docs/decisions/0029-drain-is-a-three-signal-ladder.md), `internal/runner/drain.go`

**Self-update** — the runner replacing its own binary with a newer release and
re-executing in place, when its owner has turned it on (`[update] auto` in
`config.toml`). Not **upgrade**, which is the owner's own `yad upgrade`, done
once and restarting nothing; both install a release the same way.
_Rules_: Never on a hub's say-so; the `update` control is reserved and
ignored. A release that no longer speaks a protocol major a connection syncs
over is refused. It takes over at the first idle moment, or after a drain
once 24 hours pass without one; a run is never interrupted for it, and a stop
asked for meanwhile wins. The pid stays, so a service manager sees no exit.
_See_: [0071](docs/decisions/0071-a-runner-updates-itself-when-its-owner-turns-it-on.md), `internal/selfupdate`

**Watchdog** — the runner's two timers on a run: an inactivity timeout on the
event stream, which catches a wedged harness, and an optional wall-clock cap set
per run.

## Relationships

- A **runner** has one **connection** per **hub** and any number of hubs; a hub
  sees many runners and never the others' hubs.
- A hub's own unit of work — a Zumino task, a yashiki thread — produces one or
  more **runs**. YAD never learns what produced a run.
- A **session** belongs to one runner and one harness, holds one **workdir**, and
  has many **runs**; a run belongs to exactly one session.
- A run uses one **account** at a time; a waiting run may resume on another
  account of the same harness.
- A run carries at most one **brief**, any number of **sources** and **grants**,
  and produces an ordered stream of **events** and exactly one terminal **run
  state**.

## Rules

- A run names a session, a harness and a model. None of the three is inferred
  from the runner's configuration.
- A runner never accepts a run for a harness that is not **first-class and
  present** in its current capability document, and never claims past free
  capacity.
- The runner speaks outbound only. It listens on no network port — runners sit
  behind NAT and must stay there. The only socket it opens is a Unix one for its
  own CLI.
- Absence is a fact, not a failure. A missing harness, a broken `--version`, a
  CLI that hangs — each is reported in the capability document and none prevents
  the runner from registering.
- A token is never logged, never printed, and never written anywhere but a `0600`
  file. Secrets never travel in argv — the one exception is a registration token
  typed into `yad connect`, single-use and short-lived
  ([0020](docs/decisions/0020-the-registration-token-may-be-typed.md)).
- Permission mode is the runner owner's configuration. No hub can set or widen it.
- Exit 0 is not success. A run's terminal state comes from the harness's own
  result event.
- A lost run is reported, never silently retried. A run a previous process held
  is reported lost by the next start
  ([0030](docs/decisions/0030-a-restart-reports-lost-and-replays-first.md)). Continuing a **waiting** run
  after its limit resets is not a retry — no process died, and nothing is redone.
- A terminal state is reported at least once and applied at most once.
- Harness output is data. It is streamed and stored, never acted on.
- A runner holds no work it did not claim, and no schedule of work at all. The
  one timer of its own besides its syncs and sweeps is the self-update check,
  and only when its owner turns it on.

## Open questions

- **Release** — handing a session back to its hub so it can resume on another
  runner (Anthropic's runners have it). Needs a transcript that moves; no consumer
  asks for it yet. Not the *release* `yad upgrade` installs: that one is a tagged
  build of this binary, and the two words only ever meet in this line.
- **Live** sessions: when, and what idle cost a runner accepts.
