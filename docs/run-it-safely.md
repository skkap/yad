# Run it safely

This is the guide for someone about to put a runner on a machine. It says what
a runner can do there, who decides it, and the few things worth setting up
before the first hub connects.

The short version is in the README. This is the long one, and every claim in it
is a claim about the code in this repository — where a sentence rests on a
decision record or a file, it names it, so you can check rather than believe.

## What a runner does on the machine you give it

A run arrives from a hub. The runner prepares a workdir, starts a harness —
Claude Code or Codex — and lets it work until the turn ends. Nobody is watching,
so nobody can answer a permission prompt. Both harnesses are therefore started
with their own guardrails off:

- **Claude Code** runs with `--permission-mode bypassPermissions` unless
  `permission_mode` under `[harness.claude]` in `config.toml` says otherwise
  (`internal/adapter/claude/claude.go`). It is also started with
  `--disallowed-tools AskUserQuestion`, because headless that tool returns an
  empty answer and the turn carries on as though the question had been
  answered.
- **Codex** runs with approval policy `never` and sandbox `danger-full-access`
  unless `approval` and `sandbox` under `[harness.codex]` say otherwise
  ([0036](decisions/0036-codex-runs-unsandboxed-and-never-asks-unless-the-owner-says.md),
  `internal/adapter/codex/codex.go`).

So the working assumption is the plain one: **a run can do anything the OS user
running the runner can do.** Read any file that user can read, write any file
they can write, reach any network the machine reaches, and use any credential
lying around in that user's home — git, ssh, gh, the cloud CLI you logged into
once.

Nothing in YAD narrows that. It ships no sandbox, no policy engine and no tool
allow-list, and that is a decision rather than an omission
([0015](decisions/0015-owner-environment-is-the-trust-boundary.md)): the
harnesses already ship guardrails their owner can turn on, and a second,
weaker set here would mostly give people something to trust that does not hold.

## The boundary is the machine and its owner

Everything below follows from one line: the trust boundary is the machine and
the person who owns it. Inside it, a run is as privileged as its owner. Outside
it, nothing — the runner opens no port
(`internal/control/server.go` binds one Unix socket, `0600`, in a directory that
must be private; there is no TCP listener in the runner at all), and it reaches
hubs outbound only.

`yad hub serve` is the other half of the protocol and *does* listen on a port
(`cmd/yad/cmd_hub.go`). If you run one, that is a server you are operating, and
it wants the same care any server does. A machine can be both; they are still
two parties, down to two separate databases.

## Connect only hubs you trust

This is the part people get backwards, so it is worth being blunt about.

**A hub writes the brief, and the brief is an instruction to a harness that
auto-approves.** A brief can tell the harness to read a file, send its contents
somewhere, install something, or run a command — and there is nobody to say no.
No filter on the protocol changes that, because the brief is where the power
is, not the metadata around it.

So YAD does not try
([0038](decisions/0038-the-owner-trusts-the-hubs-it-connects.md)). It does not
police what a hub sends. **A hub that can queue a run on this machine reaches
whatever this machine reaches.** Connect the hubs you would hand that much to,
and nothing else.

Two things follow, and they are the practical ones:

- **A runner is not shared between people who do not trust each other's hubs.**
  Two hubs on one runner can reach the same files and the same credentials.
- **Assume any secret the machine holds is reachable by every hub you
  connected.** Not "if something goes wrong" — by design, today.

If you want a hub's runs to reach less, the answer is a smaller machine, not a
tighter protocol.

### The guards that remain, and what they are for

0038 removed the guards that pretended to be defences against hubs. Several
remain, and they read like security rules, so it is worth saying plainly what
they now are: **protection against mistakes, not against attack.** A hub that
wanted any of this could simply ask the harness for it.

- **Grant names.** A grant may be named anything a shell accepts as an
  environment variable (`[A-Za-z_][A-Za-z0-9_]*`), except `PATH`, `HOME`, and
  the loader variables `LD_*` and `DYLD_*`, refused whatever their case
  (`protocol/v1/grant.go`). The reason is in the code next to the list: those
  four would make every run on the machine fail in a way nobody could trace.
  0024's secret-shaped suffix rule and its reserved namespaces are gone.
- **`IS_SANDBOX` is two facts that only make sense together.** It is an
  acceptable *grant name* — no namespace rule refuses it any more — and the
  Claude adapter still strips it from the run's environment before starting
  Claude (`runEnv` in `internal/adapter/claude/claude.go`). That is not a
  naming rule that survived 0038; it is 0015's line about owner configuration.
  `IS_SANDBOX` switches off Claude's own refusal to bypass permissions as root,
  so only the owner may declare it, in the runner's own environment. A hub may
  name a grant that; it cannot make it take effect.
- **Sources are argv, never a shell.** https and ssh only, no remote helpers,
  no leading `-`, no password in a URL (`internal/workdir/source.go`). Those
  prevent bugs.
- **Hub input and harness output are data.** Streamed and stored, never
  executed and never an instruction to the runner itself.
- **Tokens are never logged, printed or transmitted** beyond where they are
  delivered. A registration token typed into `yad connect` is the one
  exception ([0020](decisions/0020-the-registration-token-may-be-typed.md)).

### Where a grant actually lands

A grant reaches the harness process and nothing else: not the prompt, not the
logs, not the events, and never argv. An `env` grant is `NAME=value` in the
harness's environment. A `file` grant is a `0600` file whose path is in `NAME`,
in a directory of that run's own under `<data>/grants/`, keyed by connection
and run (`internal/runner/executor.go`). It is deleted when the run ends, and by the
next start if a crash skipped that (`internal/runner/runner.go`).

The grant directory is deliberately not inside the checkout the harness works
in, so a grant cannot be committed by accident. **It is not, however,
necessarily outside everything a run can see.** `[workdirs] roots` defaults to
your home directory when you have listed none
([0038](decisions/0038-the-owner-trusts-the-hubs-it-connects.md),
`WorkdirsConfig.EffectiveRoots`), and the data directory is normally under your
home too — so a run whose `path` source is your home has the grant directory
somewhere beneath it. Listing roots narrows both at once.

## Give it a machine of its own

A dedicated machine, a VM, or a container. The test to apply is not "do I trust
Claude" but:

> Would I let an unknown repository run code on this machine, as this user, with
> the credentials this user has?

If the answer is no, the machine is the wrong one. A personal laptop with a
logged-in cloud CLI, a production ssh key and a password manager agent is the
wrong one.

Two details that are easy to miss:

- **`claude -p` loads a repository's `.claude/settings.json` hooks and its
  `.mcp.json` servers without a trust prompt** (`ARCHITECTURE.md §8`). With
  auto-approve that is no worse than the run itself — which is exactly the
  point: the repository you check out gets to run code too.
- **A setup hook runs too.** A repository's own `.worktree/setup` is executed in
  a new workdir (`internal/workdir/hook.go`). That is a feature, and it is also
  a repository executing code on your machine before the harness starts.

## One runner per trust domain

Personal and work are two trust domains, so they are two runners. A profile is
how you get two on one machine:

```
yad --profile work connect https://…
yad --profile work doctor
yad --profile work daemon start
yad service install --profile work
```

The default profile's directories are `~/.config/yad` and `~/.local/share/yad`;
a named one lives under `profiles/<name>/` in each
(`ARCHITECTURE.md §4`). Two profiles share no configuration, no credentials, no
store, no sessions and no workdirs.

**Be clear about what that does not buy you.** Two profiles under the same OS
user are separated by directory permissions and nothing else. They run as the
same uid, so a harness started by either one can read the other's credentials,
and both of them can read that user's `~/.ssh`, `~/.gitconfig`, login keychain
and everything else that user owns. A profile separates YAD's own state. It
does not separate the machine.

## An OS user per profile

That is why the recommendation goes one step further: **give each profile its
own OS user.** Then the separation is the kernel's, not a directory mode's, and
the second runner's harnesses cannot read the first one's credentials even by
trying.

This is the shape the service install already assumes. `yad service install`
makes a *per-user* service — a launchd agent in the invoking user's GUI domain,
or a systemd `--user` unit — running as the user who invoked it. There is no
system-wide daemon and no `User=` line
([0028](decisions/0028-a-runner-is-a-per-user-service-with-its-login-path.md)).
So "one OS user per profile" is not extra machinery; it is one `yad service
install` run while logged in as each user.

Two consequences worth knowing before you start:

- **The PATH is frozen into the unit at install time**, read from that user's
  login shell. Re-run `install` after changing PATH, upgrading, or moving the
  binary.
- **On Linux a user unit stops at logout** unless lingering is on for that user.
  `install` checks and prints the exact `loginctl enable-linger` command; it
  never runs it, because lingering keeps *every* unit of that account running.

## Never root

`yad service install`, `uninstall` and `status` refuse to run as root and tell
you to run them again without `sudo` (`internal/service/service.go`). A runner
started by hand as root is not refused, but it is a bad idea for the obvious
reason — every harness it starts has root — and for one that will waste your
afternoon: **Claude Code refuses `bypassPermissions` as root**, so with the
default permission mode every Claude run fails. The adapter refuses it before
spawning, with the ways out in the message.

The one exception is a disposable container, where root is often the only user
there is. Claude's own escape hatch is `IS_SANDBOX=1` in the runner's
environment, and the adapter honours it (`ARCHITECTURE.md §3`). Use it only
where the container really is disposable: it turns off a check, it does not add
a sandbox, and the harnesses still have root inside that container.

## Keep the profile private, and let `yad doctor` tell you

The profile's directories hold credentials and harness transcripts, so YAD
creates both `0700` and writes its secrets `0600`. Things drift — a restore from
a backup, a `cp -r`, a permissive `umask`, an `rsync` that did not preserve
modes — and nothing announces it.

`yad doctor` reports it. It checks three things about the machine, alongside the
harnesses it already reported:

1. **Running as root.**
2. **The config and data directories**, for group or other permissions. (The
   credentials live in the config directory, so both are checked.)
3. **The profile's long-lived files**: `runner-id`, each
   `credentials/<connection>`, `hub-admin-token`, `state.db` and `hub.db`.

The rule for a file is the one `config.ReadSecret` already enforces before it
will hand out a credential — no group or other bits — rather than a literal
`0600`, so a credential you tightened to `0400` by hand is not scolded for it.
`config.toml` is not on the list: it holds no secret and is legitimately
world-readable, and a check that cried wolf over it would be a check people turn
off.

Here is what it looks like on a profile with a group-readable data directory and
a world-readable credential:

```
$ yad doctor
HARNESS             STATUS      VERSION                PATH
Claude Code         ready       2.1.278 (Claude Code)  /Users/me/.local/bin/claude
Codex               ready       codex-cli 0.147.0      /Users/me/.local/bin/codex
Gemini CLI          no adapter  0.29.2                 /…/bin/gemini
GitHub Copilot CLI  —
OpenCode            —
Cursor Agent        no adapter  2025.09.12-4852336     /Users/me/.local/bin/cursor-agent

warning: the data directory /tmp/yad/data is -rwxr-xr-x — another user on this machine can reach what is in it; chmod 700 /tmp/yad/data

warning: /tmp/yad/config/credentials/yashiki is -rw-r--r-- — it is the runner credential for the connection "yashiki", and one that has been readable by others must be assumed leaked; chmod 600 /tmp/yad/config/credentials/yashiki

profile default — config /tmp/yad/config
2 harness(es) this runner can be given work for.
```

Run what each warning tells you to, and it goes quiet:

```
$ chmod 700 /tmp/yad/data
$ chmod 600 /tmp/yad/config/credentials/yashiki
$ yad doctor
HARNESS             STATUS      VERSION                PATH
Claude Code         ready       2.1.278 (Claude Code)  /Users/me/.local/bin/claude
Codex               ready       codex-cli 0.147.0      /Users/me/.local/bin/codex
Gemini CLI          no adapter  0.29.2                 /…/bin/gemini
GitHub Copilot CLI  —
OpenCode            —
Cursor Agent        no adapter  2025.09.12-4852336     /Users/me/.local/bin/cursor-agent

profile default — config /tmp/yad/config
2 harness(es) this runner can be given work for.
```

**These are warnings, and `yad doctor` still exits 0.** It is the command you
run to find out what is wrong with a machine; one that refused to report the
harnesses because a directory mode was wrong would be answering a question
nobody asked. The places where the exposure would actually leak refuse on their
own: `ReadSecret` will not hand out a credential others can read, and the
control server will not bind its socket in a directory others can reach.

A credential that warned here should be treated as leaked, not just fixed:
revoke it at the hub and run `yad connect` again.

Two limits, so you know what the check does not cover. It looks at the two
directories themselves, not at their parents — a `~/.local/share` anyone can
write to is its own problem, and this will not find it. And the warnings are in
the table output only; `yad doctor --json` prints the raw harness detection
result — the same material `yad harnesses` turns into the capability document a
hub receives — and a directory's mode belongs in neither.

## Turning the guardrails back on

Everything above describes the defaults. They are the defaults because a run is
unattended and a narrower one fails the work hubs mostly send — pushing a
branch, opening a PR, installing a dependency. If your situation is different,
both harnesses have their own guardrails and you can set them, on the machine,
in `config.toml`:

```toml
[harness.claude]
permission_mode = "acceptEdits"

[harness.codex]
sandbox  = "workspace-write"
approval = "never"
```

`sandbox = "workspace-write"` keeps what Codex writes inside the run's workdir,
and cuts the network — so runs that push or install will fail. That is the
trade, and it is yours to make.

Two things about this that are not negotiable by anyone else:

- **A hub can never set or widen these.** Permission mode, sandbox, approval,
  capacity, caps and accounts are runner configuration, and no protocol field
  carries them ([0015](decisions/0015-owner-environment-is-the-trust-boundary.md)).
  This is about who decides the machine's settings, and it survives 0038
  untouched.
- **An approval request that arrives anyway is declined.** With a policy other
  than `never`, Codex asks before something it would not do on its own. Nobody
  is there, and you chose that policy meaning it, so the adapter answers no and
  says so in the run's events (`approval declined: …`). A sandbox the model can
  always talk its way out of is not a sandbox.

## A checklist, if you want one

- [ ] A machine, VM or container you would let an unknown repository run code on
- [ ] An ordinary OS user, not root — one per profile
- [ ] `yad doctor` shows the harnesses you expect, and no warning you have not
      decided about
- [ ] Only hubs you would hand this machine to are in `config.toml`
- [ ] `[workdirs] roots` set, if a run should reach less than your home directory
- [ ] `yad service install` run as that user, after PATH is what you want it
- [ ] On Linux, lingering decided one way or the other

## Where the reasoning is

- [0015 — the owner's environment is the trust boundary](decisions/0015-owner-environment-is-the-trust-boundary.md)
- [0036 — a Codex run never asks and has no sandbox, unless the owner says otherwise](decisions/0036-codex-runs-unsandboxed-and-never-asks-unless-the-owner-says.md)
- [0038 — the owner trusts the hubs it connects](decisions/0038-the-owner-trusts-the-hubs-it-connects.md)
- [0028 — a runner is a per-user service, with the owner's login PATH frozen into it](decisions/0028-a-runner-is-a-per-user-service-with-its-login-path.md)
- [0033 — sources reach only what the owner allows](decisions/0033-sources-reach-only-what-the-owner-allows.md)
- `ARCHITECTURE.md §8` is the same ground in one page, for someone reading the code.
