# Domain

The vocabulary. A word defined here means this and nothing else, in the code, in
the protocol, in the CLI and in conversation. Where this file and a comment
disagree, this file wins.

## The things

**Runner** — one `yad` process on one machine. The unit a control plane routes
to. Identified by a random id written once to `~/.config/yad/runner-id`, never by
hostname: hostnames are duplicated across cloned VMs and reassigned by DHCP, and
two machines claiming to be `ubuntu-01` breaks session affinity silently.
Deleting the id file retires the runner and creates a new one — that is the
supported way to decommission a machine.

**Agent** — a coding-agent CLI installed on the runner: `claude`, `codex`. Not a
persona, not an assistant, not a Zumino participant. When someone says "assign it
to an agent" they mean a Zumino participant; when this repo says agent it means
an executable.

**First-class agent** — one YAD has an adapter for: it knows the streaming
format, the session-resume flag, and how to cancel a turn. Only a first-class
agent may be the target of a run. Everything else in the catalog is
**recognised** — detected and reported so the gap is visible, and refused as a
target.

**Model** — what the agent is pointed at for one run (`opus`, `gpt-5.1-codex`).
Chosen per run, never per runner: the same machine serves cheap and expensive
work.

**Session** — a durable conversation with one agent in one working directory.
Has a YAD id and, underneath it, the agent's own native session id (Claude's
`--resume` id, Codex's thread id). The mapping is the entire reason sessions are
a first-class thing here: a control plane must be able to say "continue the
conversation you were having about ZUM-19" without knowing what a rollout file
is.

**Run** — one prompt executed against one session, by one agent, on one model.
The unit of work claimed, streamed and reported. A session has many runs; a run
belongs to exactly one session.

**Capability document** — what a runner advertises: its id, name, OS, arch,
labels, `yad` version, the agent list with versions, and how many runs it will
take at once. Public surface — every field is one somebody will depend on.

**Fingerprint** — a hash of the capability document with the timestamp removed.
Heartbeats carry the fingerprint; the control plane asks for the full document
only when it moves.

**Control plane** — whatever is telling this runner what to do. Zumino and
yashiki are the first two. A runner serves exactly one at a time.

**Driver** — the adapter for one control plane: how to register, how work is
claimed, where results go. `zumino` first, `yashiki` second, `http` for anything
else. Swapping drivers must not change anything below the claim.

## Invariants

- A run names a session, an agent and a model. None of the three is inferred
  from the runner's configuration.
- A runner never accepts a run for an agent that is not **first-class and
  present** in its current capability document.
- The runner speaks outbound only. It listens on no network port — the fleet is
  behind NAT and must stay there. The only socket it opens is a Unix one for its
  own CLI.
- Absence is a fact, not a failure. A missing agent, a broken `--version`, a CLI
  that hangs — each is reported in the capability document and none prevents the
  runner from registering.
- A token is never logged, never printed by `yad agents`, and never written
  anywhere but the config file at `0600`.
