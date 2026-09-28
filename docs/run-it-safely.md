# Run it safely

This is the guide for someone about to put a runner on a machine. It says what
a runner can do there, who decides it, and the few things worth setting up
before the first hub connects.

The short version is in the README. This is the long one, and every claim in it
is a claim about the code in this repository — where a sentence rests on a
decision record or a file, it names it, so you can check rather than believe.

One sentence to hold on to throughout: **whoever sets YAD up decides what
access its harnesses get, and sandboxes YAD and its harnesses themselves if
they need them confined.** YAD is not a sandbox, and nothing below changes
that; it tells you where the edges are so you can draw your own. To try a
runner out without touching the one you already have, run it in a profile of
its own ([trial.md](trial.md)).

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
  (`protocol/v1/grant.go`). The reason is in the code next to the list: these
  four would break the run rather than attack it. Each has its own — `PATH`
  chooses which executable every command runs, `HOME` moves every tool's
  configuration, and the loader variables load code into every process a run
  starts — and the one 0038 gives for keeping the list at all is that a hub's
  mistake must not unset `PATH` and make every run fail in a way nobody can
  trace back.
  0024's secret-shaped suffix rule and its reserved namespaces are gone.
- **Account names.** A grant may not name a variable that chooses whose
  credential a harness uses or which home it logs in from —
  `ANTHROPIC_API_KEY`, `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `OPENAI_API_KEY`, the
  base-URL overrides and the rest of `accountGrantNames` in the same file
  ([0040](decisions/0040-a-grant-may-not-move-a-run-off-its-account.md)). This
  is not a defence either: a brief could still tell the harness to run itself
  with any key. It keeps your accounts honest. A grant lands after the runner
  scrubs your own `ANTHROPIC_API_KEY`, so without it a hub's key would run the
  turn while the run's events, health and `yad account list` named your
  account — and that account's limits would never fire, failover would never
  move, and health would call it free for ever.
- **`IS_SANDBOX` is two facts that only make sense together.** It is an
  acceptable *grant name* — no namespace rule refuses it any more — and the
  Claude adapter still strips it from the run's environment before starting
  Claude (`runEnv` in `internal/adapter/claude/claude.go`). That is not a
  naming rule that survived 0038; it is 0015's line about owner configuration.
  `IS_SANDBOX` switches off Claude's own refusal to bypass permissions as root,
  so only the owner may declare it, in the runner's own environment. A hub may
  name a grant that; it cannot make it take effect.
- **Sources are argv, never a shell.** For a repository off the machine, https
  and ssh only — no plain http, no `git://`, no remote helpers, no leading `-`,
  no password in the URL. A repository *on* the machine, given as a path or a
  `file://` URL, is fetched too, but only from inside `[workdirs] roots`
  (`parseRemote` and `localRemote` in `internal/workdir/source.go`). Those
  prevent bugs.
- **Hub input and harness output are data.** Streamed and stored, never
  executed and never an instruction to the runner itself.
- **Tokens are never logged, printed or transmitted** beyond where they are
  delivered. A registration token typed into `yad connect` is the one
  exception ([0020](decisions/0020-the-registration-token-may-be-typed.md)).
  A hub URL with a user or password in it (`https://user:secret@hub/…`) is
  refused — by `yad connect`, and by the runner loading a `config.toml` edited
  to hold one — because it authenticates nothing: the runner sends its
  credential as a bearer token, never the URL's userinfo. Wherever yad does
  print a URL it did not choose — a refusal, a hub's redirect, an unreachable
  hub — the userinfo, query and fragment are taken out first, and a URL that
  does not parse is not repeated at all (`config.RedactURL`).
- **A hub's redirect is refused, never followed** — Go would carry the bearer
  across a same-host redirect even from https to plain http. It is refused as
  the response arrives, before Go's HTTP client parses the `Location`, so a
  `Location` too malformed to parse, which Go's own error would quote whole, is
  never repeated. Nor is a header line Go refuses to read at all, such as one
  holding a control byte (`config.HubHTTPClient`, used by the runner, the
  `yad hub` service commands and `yad conformance`).

### Where a grant actually lands

On the runner, a grant reaches the harness process and nothing else: not the
prompt, not the logs, not the events, and never argv. (On the hub it is a
different story — see the end of this section.) An `env` grant is `NAME=value` in the
harness's environment. A `file` grant is a `0600` file whose path is in `NAME`,
in a directory of that run's own under `<data>/grants/`, keyed by connection
and run (`internal/runner/executor.go`). It is deleted when the run ends, and by the
next start if a crash skipped that (`internal/runner/runner.go`). Both go
through `destroyGrants` (`internal/runner/grants.go`): when the plain removal
fails, the runner gives its own grant directories their write bit back and
tries again, and only if that fails too does it overwrite and truncate each
grant file — never through a symbolic or hard link. What can still not be
removed — a read-only mount, an immutable flag — is logged as an error at
the end of the run and again at every start, naming the directory and whether
the files there were emptied or **still hold a hub's secrets**; the one thing
that clears it is deleting them by hand.

The grant directory is deliberately not inside the checkout the harness works
in, so a grant cannot be committed by accident. **It is not, however,
necessarily outside everything a run can see.** `[workdirs] roots` defaults to
your home directory when you have listed none
([0038](decisions/0038-the-owner-trusts-the-hubs-it-connects.md),
`WorkdirsConfig.EffectiveRoots`), and the data directory is normally under your
home too — so a run whose `path` source is your home gets a checkout with the
grant directory somewhere beneath it.

**Be clear about what `roots` is, because it is easy to read as a sandbox.** It
decides which local directories a hub may name as the *material a workdir is
built from* — a `path` source, or a local git URL (`inRoots` in
`internal/workdir/source.go`). It does not confine the harness once the run
starts. Nothing does: the harness is an ordinary process running as you, and it
can read your whole home whatever `roots` says. Narrowing `roots` narrows what
a hub can ask to have checked out, and that is all it is for.

If a machine also runs `yad hub`, note that the hub's own store holds every run
it has been given, and **the grants of each one that has not ended, in
plaintext** (`internal/hub/store/migrations/0001_init.sql`). A run's spec
carries its grants until the run reaches a terminal state; then the schema
blanks every value and keeps each grant's name, so a finished run still says
what it was given and no longer holds the secret
([0041](decisions/0041-a-hub-holds-a-grant-only-while-its-run-can-use-it.md)).
A waiting run keeps its values, because its resume needs them. The database is
opened with `secure_delete`, so a blanked value does not linger in the file's
free space either. Two things widen that. In WAL mode the blanking reaches
`hub.db` itself only at a checkpoint, and `hub.db-wal` keeps the frames written
while a run was live until SQLite reuses them — so the two files together can
still hold the grants of runs that ended since the hub last stopped cleanly.
And the hub keeps every event its runners upload, which nothing blanks: a
secret a harness printed is in `hub.db` as it is in `state.db`. That is why
`yad doctor` treats an exposed `hub.db` as exposed secrets rather than exposed
state, and why the remediation is the grants of every run still in flight or
ended since the last clean stop, and anything a run printed.

## Give it a machine of its own

A dedicated machine, a VM, or a container. The test to apply is not "do I trust
Claude" but:

> Would I let an unknown repository run code on this machine, as this user, with
> the credentials this user has?

If the answer is no, the machine is the wrong one. A personal laptop with a
logged-in cloud CLI, a production ssh key and a password manager agent is the
wrong one.

`machines/` builds one: a Lima VM per runner, from a directory that says what
goes in it, with the runner's user shut out of the host, its LAN and its
tailnet ([machines/README.md](../machines/README.md)). On a Mac it is also the
way to get a second trust domain at all — see the end of "An OS user per
profile".

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
- **On macOS a second user's runner does not start by itself.** The service is
  a launchd agent in that user's GUI domain, which exists only while the user
  is logged in at the console — and a Mac logs in one user automatically. Claude
  Code also keeps its login in that user's login Keychain, which only a GUI
  login unlocks. A work machine (`machines/`) is the way round both.

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

`IS_SANDBOX` is not the only way a Claude run starts as root, and the guide
would be overstating if it said so. The adapter refuses root **only** for
`bypassPermissions`, so a narrower `permission_mode` runs as root without it —
and gives up the auto-approval the runner exists for. Neither route makes root
a good idea; they are different trades.

`yad doctor` says both of those. Run as root in a bare container with no
harness installed — note that the warning speaks only to the root question,
while whether a harness exists at all is the table above it:

```
# id -u
0
# yad doctor
HARNESS             STATUS  VERSION  PATH
Claude Code         —
Codex               —
Gemini CLI          —
GitHub Copilot CLI  —
OpenCode            —
Cursor Agent        —

warning: running as root — every harness this runner runs would have root, and Claude Code refuses the default permission mode there; run yad as an ordinary user

profile default — config /root/p/config
No drivable harness found. Install Claude Code or Codex and run this again.

# IS_SANDBOX=1 yad doctor
…
warning: running as root with IS_SANDBOX=1 — root alone no longer refuses a Claude run, but every harness this runner runs has root on this machine
```

The second run's harness table and footer are the same as the first, elided
here; only the warning changes.

## Keep the profile private, and let `yad doctor` tell you

The profile's directories hold credentials and harness transcripts, so YAD
creates both `0700` and writes its secrets `0600`. Things drift — a restore from
a backup, a `cp -r`, a permissive `umask`, an `rsync` that did not preserve
modes — and nothing announces it.

`yad doctor` reports it. It checks three things about the machine, alongside the
harnesses it already reported:

1. **Running as root.**
2. **The config and data directories** — for group or other permissions, and
   for an owner who is not you. (The credentials live in the config directory,
   so both directories are checked, not just the data one. A `0700` directory
   belonging to somebody else passes a mode check and is still theirs to read
   and replace, which is why the owning uid is compared too; the control server
   already refuses to bind a socket in one.)
3. **The profile's long-lived files**: `config.toml`, `runner-id`, each
   `credentials/<connection>`, `hub-admin-token`, `state.db` and `hub.db` — and
   each database's `-wal` and `-shm`. SQLite creates those with the database's
   own mode, so a database that drifted to `0644` hands the same bits to them;
   the `-wal` holds the pages the database has not taken yet, so an exposed
   `hub.db-wal` gives up the grants `hub.db` does — and, until SQLite reuses
   its frames, the grants of runs that have ended since, from the pages
   written while they were live. A clean shutdown removes them. **A killed
   runner does not**, so they can be sitting there when no runner is running
   at all.

The rule for a file is the one `config.ReadSecret` already enforces before it
will hand out a credential — no group or other bits — rather than a literal
`0600`, so a credential you tightened to `0400` by hand is not scolded for it.
`config.toml` **is** on the list, for the reason `config.Save` gives for writing
it `0600`: it names the hubs this runner connects to and the accounts it holds.
It carries no secret — credentials live in their own files, and a hub URL with a
password in it is refused rather than written — so the mode is the whole of its
fix, and it is not asked to be rotated.

Each file says what is at stake and what to do, and those differ — which is
why the warning is worth reading rather than skimming for the `chmod`. An
identity is closed and that is the end of it: `runner-id` cannot be used to
take a runner over, because a hub refuses to re-register an id without a token
minted for that runner, and `config.toml` names hubs and accounts but carries
no secret.

A file behind which there is a secret is closed **and** the secret retired, and
what that means is different every time. A connection's credential is revoked
at the hub and `yad connect` run again. The admin token is revoked at whichever
hub issued it — the same file is the default token for submitting to a remote
hub, so it is not always this machine's — and if it is a hub this machine
serves, the file has to be deleted between `revoke` and `create`, because
`revoke` only touches the database and `create` refuses while the file is still
there. `hub.db` means the grants of every run that has not ended or ended
since the last clean stop, and anything a run printed. `state.db` is the
conditional one: it keeps no grant, because the runner strips them before
writing, but an event body is whatever the harness printed and nothing ever
deletes one — so if a run printed a credential, it is still in there.

Here is what it looks like on a profile with a group-readable data directory and
a world-readable credential:

```
$ yad doctor
HARNESS             STATUS      VERSION                PATH
Claude Code         ready       2.1.278                /Users/me/.local/bin/claude
Codex               ready       0.157.1                /Users/me/.local/bin/codex
Gemini CLI          no adapter  0.29.2                 /…/bin/gemini
GitHub Copilot CLI  —
OpenCode            —
Cursor Agent        no adapter  2025.09.12-4852336     /Users/me/.local/bin/cursor-agent

warning: the data directory /tmp/yad/data is -rwxr-xr-x — another user on this machine can reach what is in it; chmod 700 /tmp/yad/data

warning: /tmp/yad/config/credentials/yashiki is -rw-r--r-- — it is the runner credential for the connection "yashiki"; chmod 600 /tmp/yad/config/credentials/yashiki stops the next reader, but not the one who already read it, so revoke this credential at the hub and run `yad connect` again

profile default — config /tmp/yad/config
2 harness(es) this runner can be given work for.
```

Close the directory and the file and `yad doctor` goes quiet — which is not
the same as being finished, as the paragraph after this one explains:

```
$ chmod 700 /tmp/yad/data
$ chmod 600 /tmp/yad/config/credentials/yashiki
$ yad doctor
HARNESS             STATUS      VERSION                PATH
Claude Code         ready       2.1.278                /Users/me/.local/bin/claude
Codex               ready       0.157.1                /Users/me/.local/bin/codex
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
nobody asked.

Some of what it reports is refused elsewhere and some is not, and the second
group is why it is worth running. `ReadSecret` will not hand out a credential
or an admin token others can read, and the control server will not bind its
socket in a **data** directory others can reach. Nothing refuses an exposed
`state.db`, `hub.db` or `config.toml`, and nothing but `yad doctor` looks at the
config directory at all.

**A `chmod` is only ever half the answer for a secret.** It stops the next
reader; it does nothing about whoever already read the file, and a secret that
has been readable by others must be assumed leaked — which is the same premise
`config.ReadSecret` refuses on. So a warning about a file that holds a secret
asks for the rotation as well as the mode, and names the sequence that works
for that particular one. **Read the warning rather than counting them here**:
which files those are is decided in `config.privateFiles`, and a list repeated
in prose is a list that goes stale — this sentence said "three" for a round
after `hub.db-wal` made it four.

That is also the one place this diagnostic cannot keep you honest: **the chmod
alone silences the warning.** `yad doctor` can see a file's mode; it cannot see
whether the credential behind it was ever replaced. If you skip the second
half, nothing here will tell you again.

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

`sandbox = "workspace-write"` cuts the network, so runs that push or install
will fail. That is the trade, and it is yours to make.

It does **not** confine writes to the workdir alone. In the app-server contract
this yad is pinned against, a `workspaceWrite` policy leaves `/tmp` and
`$TMPDIR` writable unless asked otherwise — `excludeSlashTmp` and
`excludeTmpdirEnvVar` both default to `false`
(`internal/adapter/codex/testdata/codex-0.157.1/codex_app_server_protocol.schemas.json`)
— and YAD sends the mode as Codex's own string, setting neither
(`internal/adapter/codex/codex.go`). Treat it as "not the whole filesystem",
not as "only the workdir".

Two things about this that are not negotiable by anyone else:

- **A hub can never set or widen these.** Permission mode, sandbox, approval,
  capacity and caps are runner configuration, and no protocol field carries
  them ([0015](decisions/0015-owner-environment-is-the-trust-boundary.md)).
  This is about who decides the machine's settings, and it survives 0038
  untouched. Accounts are the one exception, and a deliberate one: a hub may
  log an account in, add a new one and remove one
  ([0055](decisions/0055-a-hub-may-log-an-account-in-by-link-or-by-token.md),
  [0057](decisions/0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md)),
  because the person at the hub is the one holding the subscription. A new
  account is listed only once its harness's own login check says yes, and
  every hub's runs then use it. To keep one hub from adding or removing
  accounts, set `manage_accounts = false` on its `[[connection]]` in
  `config.toml` and restart the daemon; it may still log in the accounts
  already listed.
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
- [ ] `manage_accounts = false` on every connection whose hub should not add
      or remove accounts (`yad status` shows each connection's setting)
- [ ] Only hubs you would hand this machine to are in `config.toml`
- [ ] `[workdirs] roots` set, if a hub should be able to check out less than
      your whole home directory
- [ ] `yad service install` run as that user, after PATH is what you want it
- [ ] On Linux, lingering decided one way or the other

## Where the reasoning is

- [0015 — the owner's environment is the trust boundary](decisions/0015-owner-environment-is-the-trust-boundary.md)
- [0036 — a Codex run never asks and has no sandbox, unless the owner says otherwise](decisions/0036-codex-runs-unsandboxed-and-never-asks-unless-the-owner-says.md)
- [0038 — the owner trusts the hubs it connects](decisions/0038-the-owner-trusts-the-hubs-it-connects.md)
- [0028 — a runner is a per-user service, with the owner's login PATH frozen into it](decisions/0028-a-runner-is-a-per-user-service-with-its-login-path.md)
- [0033 — sources reach only what the owner allows](decisions/0033-sources-reach-only-what-the-owner-allows.md)
- `ARCHITECTURE.md §8` is the same ground in one page, for someone reading the code.
