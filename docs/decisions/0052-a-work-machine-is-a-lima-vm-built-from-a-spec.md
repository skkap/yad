---
date: 2026-09-25
status: amended by 0059 — `up` brings the machine's config.toml onto its spec on every run, keeping its connections and never removing an account
---

# A work machine is a Lima VM built from a spec, and its runner's user reaches only the internet

[0015](0015-owner-environment-is-the-trust-boundary.md) makes the machine the
trust boundary and recommends a dedicated machine, VM or container, one runner
per trust domain. It shipped the recommendation and nothing to follow it with.
The first real deployment — a runner answering a company's Slack channels from
an always-on Mac mini that also runs a personal assistant — showed that the two
cheap ways to follow it do not hold on a Mac:

- **Profiles under one user** separate yad's state and nothing else. A brief
  reaches the other profile's credentials and everything else the user owns
  (run-it-safely.md).
- **A second macOS user** keeps its runner down. `yad service install` makes a
  launchd agent in the user's GUI domain ([0028](0028-a-runner-is-a-per-user-service-with-its-login-path.md)),
  which exists only while that user is logged in at the console, and a Mac logs
  in one user automatically. Claude Code keeps its login in the login Keychain,
  which only a GUI login unlocks.

So yad ships `machines/`: a shell kit that builds a Linux VM with Lima from a
**machine spec** — a directory holding `machine.env`, the runner's first
`config.toml`, an optional `provision.sh` and `home/`. `yad-machine up SPEC`
creates or updates the VM; the logins are made by hand, once.

**Lima, created `--plain`.** No host directory is mounted and no guest port is
forwarded, and the image is a stock Ubuntu cloud image. Lima is one declarative
tool on macOS and Linux hosts, and it is what colima already runs on. Measured
on Lima 2.2.0, macOS 27, arm64: the guest reaches the host's loopback through
the gateway, `192.168.5.2` — every service there that trusts localhost — and the
LAN and the tailnet behind it.

**The runner's user is not Lima's user.** Lima's user has passwordless sudo and
is kept for provisioning. The runner runs as `agent`, with no sudo, as a systemd
user service with lingering on, so it starts with the VM and nobody logged in.
yad is installed root-owned in `/usr/local/bin`.

**`agent` reaches the internet and nothing closer.** An nftables table, matched
on the socket's owner, rejects `agent`'s traffic to private, link-local and
CGNAT ranges — the host, the LAN, the tailnet — and allows loopback, so DNS
still goes through systemd-resolved, which runs as its own user. A spec lists
what it genuinely needs in `MACHINE_EGRESS_ALLOW`. Because `agent` has no sudo, a
run cannot lift the rule. This does not confine what a hub may ask: everything
inside the machine is still reachable by every hub it connects (0038). It
confines what the machine reaches beyond itself.

**Codex is pinned to a version the adapter was recorded against.** The latest
Codex is usually newer than yad's recorded app-server protocol, and `yad doctor`
warns about it. A test in `internal/adapter/codex` fails when the kit's default
is not the one pinned, `pinnedVersion` ([0067](0067-only-the-latest-recorded-harness-protocol-is-pinned.md)).

**`config.toml` is seeded once.** After the first `up`, `yad connect` and
`yad account add` write the machine's copy; replacing it on the next `up` would
disconnect every hub. `up` reports a difference and leaves it alone.

## Considered options

**tart VMs.** Fast APFS clones and macOS guests, but an imperative setup over
SSH and no declarative definition. It was on the machine for full Linux boxes
and is not needed once Lima covers those.

**A container per runner in colima.** Lighter, but it shares a kernel with
whatever else colima runs (a CI pool, on the machine that prompted this), needs
root inside with `IS_SANDBOX`, and has no user service manager.

**A LaunchDaemon with `UserName` for a second macOS user.** It would keep the
runner up, against 0028's "never a system service", and leave the Keychain
locked all the same.

**The kit in Go, as `yad machine`.** It runs on the host, before any yad exists
in the VM, and drives two external CLIs; shell with shellcheck in `make lint`
is less code and no new dependency ([0016](0016-a-curated-set-of-dependencies.md)).
