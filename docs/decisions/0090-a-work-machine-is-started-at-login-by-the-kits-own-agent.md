---
date: 2026-10-17
status: accepted — amends 0052, whose machines registered with `limactl autostart`
---

# A work machine is started at login by the kit's own agent, which clears a stale pid and retries until it runs

Ashikaga rebooted on 2026-10-16 at 19:59 and its work machine, tl-general,
stayed down for four hours. Nothing said why: its runner went silent at the
hub, and that was all. YAD-202.

`yad-machine up` had registered the VM with `limactl autostart enable`, a
launchd agent (a systemd user unit on Linux) that runs `limactl start NAME
--foreground` once at login. The VM's last host agent never shut down, so
`~/.lima/tl-general/ha.pid` and `vz.pid` still held 742. The new boot had
given 742 to an unrelated app. Lima removes a pid file whose process is gone,
but it cannot tell a pid that has been reused: it took the app for its host
agent, failed to reach `ha.sock`, called the instance Broken, and its own
start refused it with `errors inspecting instance`. The job ran once and did
not run again. Its log shows the same thing on 2026-10-10, as `vz driver is
running but host agent is not`, so this was the third time it had happened
rather than the first. Removing the pid files and the sockets, once nothing
of Lima's was running, and then `limactl start` brought the VM back. Any
machine the kit makes can hit this on any reboot.

## Decision

**The kit registers its own agent and no longer uses Lima's.** On macOS that
is a launchd agent, `~/Library/LaunchAgents/yad-machine.NAME.plist`. On
Linux it is a systemd user unit, `yad-machine-NAME.service`. Both run
`yad-machine boot NAME` at the user's login. `boot` clears a stale pid when
Lima calls the instance Broken. It then starts the VM, and it exits 0 only
once the VM is Running, or when there is no VM to start. Any other result is
a failure, and the service manager runs it again 30 s later, with no limit on
the number of retries:

- **launchd:** `KeepAlive = {SuccessfulExit = false}` and `ThrottleInterval
  = 30`. `AbandonProcessGroup` is set, so the host agent that `limactl start`
  leaves running outlives the job.
- **systemd:** a `oneshot` unit with `RemainAfterExit`, `Restart=on-failure`,
  `RestartSec=30` and `StartLimitIntervalSec=0`. `KillMode=process` means that
  stopping or reloading the unit never takes the host agent with it.

The agent runs a copy of the kit kept at
`~/.local/share/yad-machine/yad-machine`. The kit itself is often fetched
into a cache, or run from a checkout, and either can be gone by the next
boot. Its PATH is the directory `limactl` was found in, followed by the
system's, and it carries `LIMA_HOME` when the kit was run with one. Its
output goes to `~/.local/state/yad-machine/NAME.log` on both systems, through
`StandardOutput=append:` on systemd 240 or newer. An
unchanged agent is not reloaded, because loading it runs it.

**A pid file is removed only when it does not name the instance's own Lima
process.** That process is `limactl`, a `qemu-system-*`, `krunkit` or a
`lima-driver-*` whose command line names a file in the instance's directory.
For the host agent this is `--pidfile …/NAME/ha.pid`, and on vz the host
agent is the VM. A pid that is merely alive does not count: the reused pid
was alive. **While any such process of the instance exists, nothing is
removed and nothing is started.** A host agent that is still starting looks
Broken for a moment too, and the next retry finds it Running. Once the pid
files are gone, the host agent's sockets (`ha.sock`, `*_ep.sock`,
`*_fd.sock`) go with them, since nothing can be listening on them. Every
liveness check uses `ps` rather than `kill -0`, which reports another user's
process as gone.

**One start of a machine at a time.** `boot`, `start` and `up` take a lock,
`yad-machine.lock` in the instance's directory, before they heal and start.
Without it, a start at login and one run by hand could each clear what the
other's new host agent had just written. A lock whose holder has exited is
taken over. The process table is scanned a second time before the sockets
go.

**What is printed about a process is its executable, never its arguments.**
Arguments belong to another program and may hold a secret, and what heal
prints ends up in the agent's log and in `status --json`.

**Every command the kit prints is its own, and carries `LIMA_HOME`.** A
`limactl` or `launchctl` line pasted on the laptop that `YAD_MACHINE_HOST`
reads from would act on the laptop. So the advice goes through the kit, and
the kit gained `yad-machine stop NAME [--force]` for the one case that needs
Lima's stop. `LIMA_HOME` goes in front of the command and is carried to the
host.

**`yad-machine start` heals the same way, and `status` says so.** A Broken
machine is listed with each stale pid file, the process its pid now belongs
to, and the command that heals it. `status` removes nothing. It also says
when a machine still starts through Lima's own autostart, or when nothing
starts it at all; on Linux, that is asked of systemd, since a unit file that
is not enabled starts nothing. `--json` carries `autostart` and `broken`.

**A machine an older kit built changes over when the kit is run again:**
`yad-machine up SPEC`, which is tl-yad's `./up NAME`, or `yad-machine
autostart NAME`. It runs `limactl autostart disable NAME` first, so that the
two agents never race at a login. That command disables Lima's job and
deletes its plist without unloading it. On a machine Lima started, the
running job is the host agent, and unloading it would stop the VM. Disabled,
it is never loaded again. `destroy` removes the kit's agent, and Lima's if
one is left.

## Considered options

**Keep Lima's autostart, and add a `yad-machine heal` run by a second agent at
login.** That is two agents racing at every login, and the heal would have
to finish before Lima's single attempt, which nothing orders. If Lima's
attempt failed for any other reason, nothing would try again.

**Report it to Lima and wait.** An upstream check for pid reuse in instance
inspection is the right fix, and worth filing. Machines go down on every
reboot until it ships and is installed, so the kit cannot wait for it.

**Have the agent run `limactl start --foreground` as Lima's does, with
`KeepAlive`.** The job would then be the host agent. Each `up` that changed
the agent would have to unload it, which stops the VM. `limactl stop`
would be undone at once, unless the exit status could be trusted to tell a
stop from a crash. A start that returns once the VM is up, retried until it
is, has neither problem.

**Judge each pid file on its own, without scanning the process table.** A
Lima process of the instance whose pid file is missing, or not written yet —
a host agent mid-start, one started by hand — would then have its sockets
removed from under it. The scan is one `ps`.
