---
date: 2026-09-19
---

# A runner is installed as a per-user service, with the owner's login PATH frozen into it

`yad service install` makes one profile's runner a service of the machine's own
manager: a launchd agent in the owner's GUI domain on macOS, a systemd
`--user` unit on Linux. Either one runs `yad --profile P daemon start --foreground`
as the invoking user. Five things about that were not settled by
ARCHITECTURE.md §5.

**Never root, and never a system service.** A runner runs its harnesses as the
user who owns it (§8), and Claude refuses to bypass permissions as root. So
install, uninstall and status refuse to run as root and tell the owner to run
them again without sudo. There is no LaunchDaemon or system unit with a `User=`
line. Those would need root to install, and they would split the service from
the user whose homes, logins and keychain the harnesses need.

**PATH is read from the owner's login shell at install time and written into
the unit.** A service starts with launchd's or systemd's minimal PATH, which has
no `~/.local/bin`, Homebrew or version-manager shims. A runner started with that
PATH detects no harness, and a harness it does start finds no git or node.
Install runs `$SHELL -ilc` (falling back to `-lc`), bounded by a timeout, and
reads a marked PATH out of whatever the rc files print. It keeps the whole PATH,
not just the harness directories, but drops empty and relative entries. When
the shell is not a POSIX one or cannot be read, install uses its own PATH and
says so. Running install again refreshes the PATH. The overrides that decide a
profile's directories (`YAD_CONFIG_DIR`, `YAD_DATA_DIR`, `XDG_CONFIG_HOME`,
`XDG_DATA_HOME`) are carried the same way. Nothing else from the environment is
carried: in particular no token.

**Names carry no person and no domain.** The launchd label is
`yad.runner.<profile>` and the systemd unit is `yad-runner-<profile>.service`.
The default profile is `default`. There is one unit per profile, so profiles
coexist.

**Install is idempotent: it replaces.** It unloads a loaded job, writes the new
file atomically and loads it again. On launchd that is bootout, enable and
bootstrap. On systemd it is daemon-reload, enable and restart. Re-running
install after upgrading or moving the binary is the fix, not an error.
Uninstall stops the runner, removes the file and succeeds when there is nothing
to remove.

**A crash is restarted with a pause, and a clean exit stays stopped.** launchd
has `KeepAlive.SuccessfulExit = false` and a `ThrottleInterval` of 10 s.
systemd has `Restart=on-failure`, a `RestartSec` of 10 s growing to 5 min, and
no start limit. A clean exit is the owner stopping the runner, and restarting
it would leave no way to stop it short of uninstalling. The stop timeout is
30 s: runs in flight are held across a stop and settled at the next start, so a
stop only needs to flush the spool. Drain (DEV-13) is what waits for runs.
systemd uses `KillMode=mixed`, so the runner, not systemd, stops its harnesses.

Standard output and error go to `logs/service.log` in the profile's data
directory under both managers, next to the daemon's own log (DEV-11). That file
catches what the runner prints before its logger is up, and a panic.

**Lingering is the owner's decision.** A systemd user manager stops at logout
unless lingering is on for the user. Install checks with `loginctl` and, when
lingering is off, prints what that means and the exact `loginctl enable-linger`
command. It never runs it, because lingering keeps every user unit of that
account running, not only yad's.

## Considered options

**Resolve harnesses by the login shell at every start**, which is what Multica's
daemon does on each probe. PATH changes would take effect without a re-install,
but every start and every probe would run the owner's rc files, and those can
hang, prompt, or change between logins. A PATH frozen into the unit at install
is visible (`plutil`, `systemctl cat`) and reproducible.

**Only the harness directories on PATH.** This is smaller, but harnesses shell
out to git, node, gh and the rest, so the whole login PATH is what they expect.

**`com.<domain>.yad` labels.** yad has no domain of its own to put in reverse
DNS, and an owner's or author's name has no place in a product.

**journald for systemd logs.** It is native and rotated, but it would make the
log's location differ between the two platforms, and `yad daemon logs` would
then need two readers.
