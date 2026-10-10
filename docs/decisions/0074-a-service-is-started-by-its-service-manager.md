---
date: 2026-10-11
---

# A runner that is a service is started by its service manager

On a Mac whose runner was the launchd job `yad.runner.default`,
`yad daemon restart` stopped the job's daemon and started a detached one in
its place (DEV-173). `launchctl list` then showed the job with no pid. A clean
stop is an exit KeepAlive leaves alone (0028), so launchd was watching
nothing, and a crash of the new daemon would have stayed down. systemd's
`Restart=on-failure` leaves the same gap.

So `yad daemon start` and `yad daemon restart` ask the service manager first.
When the profile's runner is its unit, the daemon is started by the manager:
`launchctl kickstart gui/<uid>/yad.runner.<profile>`, or
`systemctl --user start yad-runner-<profile>.service`. The command then waits,
as a background start does, for a daemon that answers ready on the control
socket. A restart stops the old daemon exactly as before, over the socket with
`--timeout` and `--force` (0027), and the clean exit leaves the unit stopped
for the manager to start again.

**Which unit is the profile's.** A label is only a profile's name, and a unit
of that name may run the same profile name under other directories: a
`YAD_DATA_DIR` set in one shell and not the other, or a test's throwaway
profile on the owner's own machine. So a unit is this profile's runner when
the manager's running job is the pid holding the profile's lock, or when the
installed unit's log file is this profile's `logs/service.log`. That path is
under the data directory, which is what the lock is in too. A unit the manager
has not loaded (booted out) is not started through it. The start goes ahead
outside it and says how to put the runner back.

**When the manager cannot be asked.** An SSH session with no GUI domain, or a
missing user bus, means `launchctl print` or `systemctl --user show` fails.
With no unit file there, that is no service: the detached start, as on a
machine with no service manager at all. With the unit file there, the profile
may be a service, and either guess repeats DEV-173 or refuses a start that was
fine. So start and restart refuse, leave the running daemon alone, and print
the manager's own command (`launchctl kickstart [-k] …`,
`systemctl --user start|restart …`) for the owner, who can tell.

`--interval` is refused for a service, before anything is stopped: the unit
runs the command it was installed with, and nothing typed here reaches it.

`yad daemon stop` is unchanged. Its clean exit is what both units leave
stopped. A `--force` that ends in SIGKILL exits non-zero, so the manager
starts the runner again; that is the manager doing its job on a killed
process, and is the owner's to undo with `yad service uninstall`.

## Considered options

**`launchctl kickstart -k` and `systemctl --user restart` for the whole
restart.** These are one call, but the stop becomes the manager's SIGTERM
instead of the ask-then-signal stop of 0027. The wait is then bounded by the
unit's stop timeout instead of `--timeout`, and `--force` means nothing. The
stop the owner already knows, followed by the manager's start, keeps one stop
path for every daemon.

**Refusing to restart a service and naming the command.** This is safe, but
the command is one yad can run itself, and the owner then has two restarts
to remember.

**Telling the unit apart by an environment variable the daemon reports**
(launchd's `XPC_SERVICE_NAME`, systemd's `INVOCATION_ID`). It identifies a
running daemon only. The case in DEV-173 is a unit with nothing running under
it.

**`yad service install` as the restart**, which `yad upgrade` used to offer.
It rewrites the unit and refreshes PATH, which a restart should not do, and on
a profile that was never a service it creates one.
