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
| `config.toml` | The runner's [configuration](../ARCHITECTURE.md): name, labels, capacity, harness settings. Copied in on the first `up` only — after that `yad connect` and `yad account add` write to the machine's copy. |
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
machines/yad-machine destroy NAME               delete the VM and everything in it
```

`up` is idempotent. Run it again after changing the spec, and to take another
yad: `--yad PATH` installs a linux binary you built (`make dist` writes
`dist/yad-linux-arm64` and `-amd64`); without it, `up` fetches the latest
release, or `YAD_VERSION`'s, and checks it against the release's
`checksums.txt`. `YAD_REPO` points at a fork.

`up` also registers the VM to start at the host user's login
(`limactl autostart`) and protects it from `limactl delete`. `destroy` undoes
both after you type the name.

**From another computer.** With `YAD_MACHINE_HOST=<ssh host>`, every command
runs on that host through its own `yad-machine` — put the kit on its `PATH`
(`ln -s …/machines/yad-machine ~/.local/bin/`) — so a laptop can see and log in
the machines on a server:

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
