---
date: 2026-09-30
---

# A runner updates itself when its owner turns it on

[0018](0018-no-self-update-in-v1.md) held self-update back: a machine must
not replace its own binary on a remote's say-so, and there was no fleet to
justify it. The fleet has arrived — work machines built from a spec, several
per host, each one `yad upgrade` and a restart away from the last release — and
the remote's say-so is still the thing to keep out. So self-update is built,
and it is the owner's alone.

## Decision

The owner's (2026-09-30), five decisions, and what building them settled.

- **Off unless `config.toml` turns it on**: `[update] auto = true`. No
  protocol field reaches it, so no hub can turn it on or widen it, and the
  reserved `update` control stays what 0018 made it — **explicitly ignored**:
  the runner never acts on it, and a test holds that. `yad config apply`
  carries the setting like any other, so on a work machine the spec decides
  (0059). `yad-machine up` refuses a spec that turns it on while `YAD_VERSION`
  pins a release: the pin says one release, the setting says the newest, and
  the runner would settle it within hours without saying so.
- **The source is the one `yad upgrade` uses** — the repository's GitHub
  releases, `YAD_REPO` for a fork — and the download is verified exactly as
  `yad upgrade` verifies one, because it is `internal/upgrade.Apply` that does
  it: into a directory beside the binary, SHA-256 against the release's
  `checksums.txt`, one rename(2). A check is **one anonymous request**, the
  `/releases/latest` redirect `yad upgrade` reads, which is not
  api.github.com's shared limit. It runs **every six hours, jittered by a
  tenth**, the first ten minutes after a start. A build with no release
  version (`dev`) checks nothing and says so in `yad status`.
- **Any newer release, but never one that drops a protocol major a
  connection syncs over.** Between the checksum and the rename, the runner
  runs the downloaded binary's `yad version --json` — with `HOME` and
  nothing else of its environment — which names the majors it speaks in a new
  `protocols` field. A release missing one a connection uses is refused: the
  binary stays as it is, the log and `yad status` say which major, which
  connections and what to type once the hubs speak it, and the same release is
  not downloaded again for the life of the process. Every connection a v1
  build holds syncs over v1. **A release older than `--json` ignores the flag
  and prints its one line; it is read as v1-only**, which every release before
  the field was — a fact, not a guess. A binary that does not call itself
  newer than the running one is refused too, since taking it would fetch it
  again at every check. A binary that does not answer in thirty seconds is a
  failure, not a refusal, and the next check tries again.
- **The takeover keeps working until it can stop without interrupting
  anything.** The release is installed on disk at once. The running process
  becomes it at the **first idle moment** — every unit of capacity free, so no
  run held, no claim awaiting its hub, no sync holding units to offer, and no
  hub login waiting on a person — by a drain, which with nothing held ends in
  moments. If no idle moment comes within **24 hours**, the same drain begins
  anyway: no new work, and the runs held finish **however long they take**.
  This drain is the one kind with no drain wait, because a run is never
  interrupted for an update. At its end the daemon closes its control socket
  and lock, `state.db` and its log, and `exec`s the same path with the same
  arguments and environment: the pid stays, so launchd and systemd see no
  exit and restart nothing. The spool, the outbox, the sessions and parked
  runs are in `state.db`, and the new process starts from them as any start
  does.
- **No release signing.** The checksums come from the same release as the
  binary, so they catch corruption and not a compromised release. Signing
  needs a dependency, which is a separate decision.

What building it settled:

- **A stop wins over an update.** The owner's `yad daemon stop`, a stop
  signal or a hub's `drain` during an update's drain turns it into an
  ordinary one: the drain wait starts then, and the process exits instead of
  re-executing, since each of them asked for a runner that is not taking work.
  Its next start is the new binary.
- **An exec that fails exits the process with the reason.** The service
  manager's restart-on-failure then starts the binary already in place, which
  is the new one.
- **On a work machine, turning it on gives the binary to the runner's user.**
  The kit installs yad root-owned in `/usr/local/bin`, so a run cannot replace
  the runner. A runner that updates itself has to be able to, so with
  `auto = true` the kit installs it in the agent's `~/.local/bin` with
  `/usr/local/bin/yad` a link to it, and turning it off puts the root-owned
  file back. This guarded less than it looks: the agent can already rewrite
  its own systemd unit, which names the binary the runner starts.

This replaces 0018's "no self-update in v1". The rest of 0018 stands: a hub
may declare a minimum version and refuse an older runner with `yad upgrade`
as the next action, and `update` stays a reserved control name.

## Considered options

**A hub-sent `update`**, which 0018 reserved the name for: the remote's
say-so over what runs on someone else's machine, which is the thing to keep
out, and a hub has nothing to add — the release is public.
**Staging the download and renaming at the takeover**, so the file on disk
stays the running version until then: a staged file outlives a crash and has
to be swept, and a crash before the takeover would restart the old binary,
only for the next check to download the same release again. Installing at
once is what `yad upgrade` does, and a restart for any reason then lands on
the new release.
**Exiting and letting the service manager start the new binary**: a clean
exit is an owner's stop to launchd and systemd (0028), which do not restart
it, and a runner started by hand has no manager at all.
**Holding claims without draining while waiting for idle**: a second way of
not claiming beside the drain, with its own quiescence to get right, for no
behaviour the update drain does not already give.
