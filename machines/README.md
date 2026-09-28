# Work machines

A **work machine** is a Linux VM that holds one yad runner and nothing of the
host it runs on. `yad-machine up SPEC` builds one from a directory that says
what goes in it — its size, its tools, the files in its home, the runner's
configuration — so a second one, or the same one rebuilt, is one command and a
few logins away.

It is the shape [docs/run-it-safely.md](../docs/run-it-safely.md) recommends,
one runner per trust domain, made concrete for a Mac or Linux host with
[Lima](https://lima-vm.io). Why it is a VM and not a profile or a second OS
user is in [0052](../docs/decisions/0052-a-work-machine-is-a-lima-vm-built-from-a-spec.md).

```
host (macOS or Linux, limactl)
└── Lima VM "tl-general" — Ubuntu 24.04, created --plain: nothing mounted, nothing forwarded
    ├── Lima's user        passwordless sudo — provisioning only
    └── agent              no sudo — runs the runner
        ├── yad-runner-default.service   systemd user unit, lingering: up whenever the VM is
        ├── ~/.local/bin/claude, codex   the harnesses, logged in once by hand
        ├── gh                           the machine's own GitHub identity
        └── ~/AGENTS.md, ~/.claude/…     the spec's home/, laid over the home
```

## What a machine can reach

The runner's user, `agent`, reaches **the internet and nothing closer**. An
nftables table rejects its traffic to private, link-local and CGNAT addresses,
so it cannot reach:

- **the host.** Lima's gateway, `192.168.5.2`, forwards to the host's loopback —
  every service there that trusts localhost.
- **the LAN.**
- **the tailnet.**

Loopback inside the VM stays open, and DNS works. Lima's own user is not held
by the rule; it provisions the machine, and a run cannot become it — `agent` has
no sudo.

What this does **not** do: confine what a hub may ask. A brief can tell a
harness to use anything inside the machine — every credential you log in there,
every file in the home — and the harness auto-approves (run-it-safely.md). Put
in a machine only what every hub it connects may have, and prefer credentials
that can only read.

## A spec

A directory. `example/` is one to copy.

| file | |
|---|---|
| `machine.env` | Shell assignments: `MACHINE_NAME`, `MACHINE_CPUS`, `MACHINE_MEMORY` and `MACHINE_DISK` (GiB), `MACHINE_HARNESSES` (`claude`, `codex`), `MACHINE_EGRESS_ALLOW` (private addresses to allow), `NODE_MAJOR`, `CODEX_VERSION`. |
| `config.toml` | The runner's [configuration](../ARCHITECTURE.md): name, labels, capacity, harness settings, accounts. Every `up` brings the machine's copy onto it, keeping the machine's hub connections and its accounts — see [Changing a spec](#changing-a-spec). |
| `provision.sh` | Optional. Runs as root after the kit's own provisioning, on every `up`: the packages and tools this machine's work needs. Must be idempotent. |
| `home/` | Laid over `agent`'s home on every `up`, never mirrored: `AGENTS.md` for the harnesses, `.claude/CLAUDE.md`, `.claude/settings.json`, `.codex/AGENTS.md`, skills, a `.gitconfig`. |

A spec holds **no secrets** — it is meant to live in a repository. The
credentials a machine needs are logged in by hand, once, and a spec says which
they are in a README of its own.

`home/AGENTS.md` is where a machine tells its harnesses what they are for,
what is here and what must never be done. The example imports it into Claude's
user memory (`home/.claude/CLAUDE.md` holds `@~/AGENTS.md`) and links it as
Codex's (`home/.codex/AGENTS.md`). Those are the harnesses' default homes — a
harness given a yad account (`yad account add`) runs from the account's own home
instead, and does not read them.

## Commands

```
machines/yad-machine up SPEC_DIR [--yad PATH]   create or update the machine
machines/yad-machine status [NAME] [--json]     every machine, or one: runner, hubs, logins, checks
machines/yad-machine login NAME                 make every login the machine is missing, one by one
machines/yad-machine shell NAME [COMMAND...]    a login shell, or a command, as agent
machines/yad-machine start NAME                 start a machine that is stopped
machines/yad-machine autostart NAME             start it at the host user's login again
machines/yad-machine destroy NAME               delete the VM and everything in it
```

`up` is idempotent. Run it again after changing the spec, and to take another
yad: `--yad PATH` installs a linux binary you built (`make dist` writes
`dist/yad-linux-arm64` and `-amd64`); without it, `up` fetches the latest
release, or `YAD_VERSION`'s, and checks it against the release's
`checksums.txt`. `YAD_REPO` points at a fork.

`up` restarts the runner only when it has to — `config.toml` changed, a
different yad was installed, or the runner was not running — since a restart
drains the runs it holds. An `up` that changes nothing restarts nothing.

`up` also registers the VM to start at the host user's login
(`limactl autostart`) and protects it from `limactl delete`. `destroy` undoes
both after you type the name.

**From another computer.** With `YAD_MACHINE_HOST=<ssh host>`, every command
runs on that host through its own `yad-machine` — put the kit on its `PATH`
(`ln -s …/machines/yad-machine ~/.local/bin/`) — so a laptop can see and log in
the machines on a server. Every command they print is written for where it is
read — `YAD_MACHINE_HOST=… <the laptop's yad-machine> …` — so it runs as pasted
on the laptop:

```bash
YAD_MACHINE_HOST=ashikaga machines/yad-machine status
YAD_MACHINE_HOST=ashikaga machines/yad-machine login tl-general
```

## The first time

```bash
machines/yad-machine up path/to/spec            # 5–10 minutes the first time
machines/yad-machine login tl-general           # each login it is missing
machines/yad-machine shell tl-general           # then, as agent:
yad connect <hub url> --token -                 # the hub's registration token, pasted
yad service install                             # restart the runner so it claims work
machines/yad-machine status                     # every machine, and what each needs
```

## Knowing what is logged in

`yad-machine status` asks each machine and prints one row per machine, then
everything that needs a person, each with what to do:

```
MACHINE     RUNNER   HUBS            LOGINS          CHECKS
tl-general  running  zumino syncing  claude/tl free  github ok
```

`--json` prints one array with an entry per machine asked about, whatever state
it is in — `vm` says `Running`, `Stopped` or `gone`, and `report` is what the
machine said, or null when it did not answer. A part of a report the machine
could not answer is null too, and `status` shows it as `unknown` rather than
guessing.

Every answer is yad's own: the runner service, `yad status` (its hubs and
whether each is syncing), `yad doctor` and `yad account list` (each harness and
account: `free`, `limited`, `needs_login`, or a harness on its own login that
has none), and the capability warnings a hub also sees, such as a token a month
from expiring. Hubs see the same states, so a machine that cannot take a run
also stops being offered one.

A spec adds **checks** for what yad does not know about — a GitHub identity, a
database: each executable in `home/.config/yad-machine/checks/` is run as
`agent`, and its exit status and first line are its answer. The first line
says what is wrong and what to do about it, and never holds a secret.

List the accounts a machine runs on in its `config.toml`
(`[harness.claude] accounts = ["main"]`). Each is then reported as
`needs_login` from the first `up`, instead of the machine looking ready and
failing its first run.

## Logging in

`yad-machine login NAME` goes through every login the machine is missing and
nothing else:

- **Claude** — paste a token from `claude setup-token`, run on any computer with
  a browser. It lasts a year, needs no browser on the machine, and yad keeps it
  in the account's home and hands it to the account's runs
  ([0054](../docs/decisions/0054-a-claude-account-may-be-a-token-and-every-account-shares-the-machines-config.md)).
  Or press Enter for Claude's own login: a link to open elsewhere and a code to
  paste back. yad warns a month before a token's year is up.
- **Codex** — a link and a code to enter on any computer (`--device-auth`).
- **A failing check** — what it says is printed, since only the spec knows how
  to fix it.

Make one token per machine rather than copying one around, so revoking a
machine's access touches no other. Scripts can skip the prompts:
`yad-machine shell NAME yad account add claude main --token -` with the token
on stdin.

**From the hub**, once the machine's runner is connected to one that supports
it ([0055](../docs/decisions/0055-a-hub-may-log-an-account-in-by-link-or-by-token.md)),
no shell on the machine is needed. A hub shows **Log in** beside an account
its runner reports as `needs_login`: follow the link and paste the code back,
or paste a `claude setup-token` token — for Codex, follow the link and type
the code the hub shows there. The runner runs the harness's own login in the
account's home, or stores the token there, and the account is back in service
once the harness's own check says so.

A hub can also **add** an account — a second subscription, say — and
**remove** one ([0057](../docs/decisions/0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md)).
An add is a login for a new label: the runner writes it into the machine's
`config.toml` only once the login takes, and a login that does not take adds
nothing. A removal is `yad account remove`'s: runs already on the account
finish there, and its login is deleted when the last one ends. An account a
hub added is the machine's like any other — every hub the runner syncs with
runs on it, and `yad account list` shows it. The first account added to a
harness takes over from that harness's own login on the machine. With `yad
hub` as the hub:

```bash
yad hub login start <runner> claude main    # prints the link, then reads the code
yad hub login token <runner> claude main < token.txt
yad hub login start <runner> codex main     # prints the link and the code to type there
yad hub login start --add <runner> claude second   # adds an account, by link
yad hub account remove <runner> claude second
```

Every hub the runner connects to may do this unless you say otherwise, per
connection, in `config.toml` — read when the daemon starts, so restart it
after a change:

```toml
[[connection]]
name = "work-hub"
url  = "https://hub.example.com/yad/v1"
manage_accounts = false   # this hub may log accounts in, but not add or remove them
```

A ChatGPT Business, Enterprise or Edu workspace has device-code login off
until its admin turns it on; until then a Codex login from the hub ends
`failed` saying so.

## Changing a spec

Edit the spec and run `up` again. The machine's `config.toml` is brought onto
the spec's by `yad config apply`, which prints each setting it changed:

```
--> yad config.toml
changed /home/agent/.config/yad/config.toml to match /var/tmp/yad-machine/spec/config.toml:
  harness.claude.accounts = ["main", "tl"] (was ["main"])
  labels = ["linux", "tl"] (was ["linux"])
a running runner reads it when it starts — `yad --profile default service install` restarts it
--> runner service — config.toml changed
```

- **The spec wins** for every setting it can hold — name, labels, capacity,
  each `[harness.*]`, `[sessions]`, `[drain]`, `[workdirs]` — and a setting
  it leaves out goes back to yad's default. Change these in the spec, not on
  the machine: an edit made there is undone by the next `up`, which says so.
- **Connections stay the machine's.** `yad connect` made them with a
  credential that is only there; a `[[connection]]` in a spec is not taken.
- **Accounts are added, never removed.** Each account the spec lists is
  added to the machine's list, first and in the spec's order; an account
  only the machine lists — logged in there, or added by a hub — is kept,
  after them. Taking an account out of the spec does not remove it from the
  machine: `yad-machine shell NAME yad account remove claude old` does, or a
  hub's remove. A new account the spec lists shows as `needs_login` until
  `yad-machine login` makes it.

Why it is shaped this way is in
[0058](../docs/decisions/0058-up-brings-a-machines-config-onto-its-spec-and-never-removes-an-account.md).
A yad older than `yad config apply` — an old `YAD_VERSION` — leaves the
machine's copy alone as before, and says which newer yad does it.

## Rebuilding

A machine is disposable: `destroy`, then `up`, then the logins again. What is
lost is what was only ever in the machine — the logins, the runner's credential
(so the hub sees a new runner, and the old one is removed there), its open
sessions and workdirs. Everything else comes from the spec.

## Requirements

Lima 2.x — measured on Lima 2.2.0, macOS 27 on Apple silicon, with the
`ubuntu-24.04` template. On a Linux host Lima needs KVM. `MACHINE_IMAGE` in
`machine.env` names another Lima template, but the provisioning expects apt and
systemd.
