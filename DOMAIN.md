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
_Avoid_: using it for per-run harness settings — there are none in the protocol
_See_: `internal/config`

**Hub** — whatever gives a runner work: anything that hosts the server half of
the runner protocol. A role, not a product — Zumino becomes a hub by embedding
it, yashiki by embedding it or by using `yad hub`, which is the standalone one.
_Avoid_: control plane, master, server, upstream, provider, dispatcher
_Rules_: A hub is untrusted input. Nothing a hub sends can widen what the
runner's owner configured.
_See_: [0003](docs/decisions/0003-hub-is-a-role-runner-is-multi-homed.md), `internal/hub`

**Connection** — one runner's standing registration with one hub: the hub URL,
the runner credential, and the owner's caps for it. A runner has one connection
per hub and any number of hubs.
_See_: `internal/config`

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
_Avoid_: capabilities for anything else — yashiki's "capabilities" are its house
tools, which here are **host tools**
_See_: `internal/capability`, `protocol/v1`

**Fingerprint** — a hash of the capability document with the timestamp removed.
Syncs carry the fingerprint; a hub asks for the full document only when it moves.
_Avoid_: using it for detection (Nomad's "fingerprinting") — here it is only the hash

**Labels** — free-form routing strings the owner attaches to a runner
(`linux`, `gpu`, `work`). Hubs route on them; YAD never interprets them.

**Host tools** — the non-harness executables a run may need and the owner has
installed and logged in: `gh`, `git`, `docker`, the `zumino` CLI. Probed and
advertised so a hub can route "needs gh + docker" to a runner that has them.
_Rules_: Long-lived credentials behind host tools belong to the machine and never
travel in the protocol.

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
home (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`). Configured by the owner, in order, per
harness. Hubs see an account's label and limit state, never its credentials.
_See_: [0013](docs/decisions/0013-accounts-fail-over-and-limited-runs-wait.md)

**Usage limit** — a subscription window an account has exhausted: Claude's
five-hour and weekly limits, Codex's primary and secondary windows. Has a reset
time.
_Avoid_: rate limit for this. A **rate limit** is transient API throttling the
harness retries by itself (`system/api_retry`); it never makes a run wait.

**Model** — what the harness is pointed at for one run (`opus`, `gpt-5.1-codex`).
Chosen per run, never per runner: the same machine serves cheap and expensive
work.

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
harness. A session **closes** — by its hub's word, its owner's, the idle TTL
or disk pressure — only while no run is held in it, takes no new run after, and
its hub is told why ([0035](docs/decisions/0035-a-runner-reports-every-close-in-its-sync.md)).
_States_: open | closed (its hub or its owner closed it) | expired (the idle TTL
or disk pressure did)
_Avoid_: thread, conversation, chat — as names for this; Codex's "thread" is the
native id underneath
_See_: [0007](docs/decisions/0007-sessions-map-never-wrap.md), [0031](docs/decisions/0031-a-failed-resume-is-the-hubs-to-decide.md), `internal/store`

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

**Brief** — what a run is told. Two parts: the **context**, appended to the
harness's system prompt so it survives compaction, and the **instruction**,
which is the run's one user turn.
_Avoid_: prompt, for the whole thing — the prompt is only the instruction

**Sources** — the material a run's workdir is built from: git repositories
(URL, base ref, branch) or an existing local path. Optional — a run that answers
a question or uses host tools has none.
_Rules_: A source is hub input. It reaches the network over https or ssh with
the machine's own credentials, and the machine itself only inside the roots the
owner allows — none by default. A session keeps the sources its workdir was
built from; a run continuing it names the same ones or none.
_See_: [0033](docs/decisions/0033-sources-reach-only-what-the-owner-allows.md), `internal/workdir`

**Workdir** — the directory a session's runs execute in. Owned by the session,
kept between its runs, reclaimed after it closes. Built from the sources, or
empty when there are none.
_Avoid_: workspace — the tenant in Zumino and in Multica
_See_: [0011](docs/decisions/0011-hub-closes-sessions-runner-collects.md), [0032](docs/decisions/0032-a-workdir-belongs-to-its-session.md), [0035](docs/decisions/0035-a-runner-reports-every-close-in-its-sync.md), `internal/workdir`, `internal/runner/collect.go`

**Setup hook** — a repository's own `.worktree/setup`, which YAD runs in a new
workdir with the `WT_*` variables, exactly as `gpiwt` does. Optional; a repo
without one still runs, and the run's events say so.
_Rules_: A hook that fails fails the run while it is preparing; the session's
next run tries it again.
_See_: `~/my/gpi-tools/docs/worktrees/README.md`, [0034](docs/decisions/0034-a-failing-setup-hook-fails-the-run.md)

**Slot** — `WT_SLOT`: a small integer unique among one repository's live
worktrees on a machine, from which the setup hook derives ports and container
names.
_Avoid_: slot for a unit of capacity

**Grant** — a short-lived secret a hub attaches to one run, scoped to it — a
Zumino token limited to one task. Delivered in the environment or a `0600` file,
never argv, and destroyed when the run ends.
_See_: [0009](docs/decisions/0009-machine-owns-credentials-hubs-grant-per-run.md)

### The lifecycle

**Sync** — the runner's periodic request to one hub. One call is the heartbeat,
the lease renewal for every run it holds, the health report and the ask for
work; the answer carries new runs (never more than free capacity), control
messages and the interval until the next sync.
_Avoid_: poll and heartbeat as separate things — there is one call
_See_: [0005](docs/decisions/0005-pull-by-periodic-sync.md)

**Claim** — a runner taking a run offered in a sync. **Lease** — the hub's
promise that a claimed run is this runner's for now; each sync renews it, and a
lease that lapses makes the run **lost** from the hub's side.

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
wait, then cancels them. The hub's `drain` control is the first step.
_See_: [0029](docs/decisions/0029-drain-is-a-three-signal-ladder.md), `internal/runner/drain.go`

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
- A runner holds no work it did not claim, and no schedule at all.

## Open questions

- Do both CLIs resume a session from a transcript directory shared between
  account homes? Assumed by
  [0013](docs/decisions/0013-accounts-fail-over-and-limited-runs-wait.md);
  unverified. If not, a session is pinned to its account.
- Do Anthropic's consumer terms allow failing over between several personal
  subscriptions? Team or Enterprise seats and genuinely separate accounts are
  different cases.
- **Release** — handing a session back to its hub so it can resume on another
  runner (Anthropic's runners have it). Needs a transcript that moves; no consumer
  asks for it yet.
- **Live** sessions: when, and what idle cost a runner accepts.
- How Zumino hosts the hub half. Its current rules forbid an executor registry;
  the operator called them temporary.
- LINEMO is named as the first real use; nothing in the repos describes it yet.
