---
date: 2026-10-12
---

# A self-update vets a release against every profile on its binary

[0071](0071-a-runner-updates-itself-when-its-owner-turns-it-on.md) let a
runner vet a release against its own profile's connections and then rename
it over the binary every profile on the machine runs. A second profile whose
hubs sync over a major the release dropped would start it at its next start
and lose them. With v1 the only major there has been, nothing could differ
yet; the first v2 would. Profiles could not see each other, because each may
keep its directories anywhere (`YAD_CONFIG_DIR`, XDG). The binary is the one
thing they share, so that is where they meet.

## Decision

The owner's (2026-10-12): **each profile records what it needs in a
directory beside the binary** — `.yad-needs/<hash of its config
directory>.json`, beside the file `os.Executable` names once symlinks are
resolved, which is the file a release is renamed over. So
`/usr/local/bin/yad`, linked to `~/.local/bin/yad` by the work-machine kit,
keeps its records in `~/.local/bin`.

What building it settled:

- **Every daemon writes, whether or not its own self-update is on**, since
  another profile's update replaces its binary too. A record holds the
  profile's config directory (absolute — its identity, and how its liveness
  is told), its name, each protocol major its connections sync over with the
  connections that do (empty, never absent, for a hubless profile), and the
  yad that wrote it, when. It is written at start, in one rename, `0644`:
  nothing in it is secret, and an updater running as another user of the
  binary has to read it. Beside a bin directory a group may write, the
  needs directory takes that group and its write, with the sticky bit as
  `/tmp` has it: each member may replace the binary, so each has to be able
  to record beside it, whoever made the directory first.
- **The record follows `config.toml`, not the process.** Once a minute the
  daemon looks at `config.toml` and writes again when its connections have
  changed or the record has gone missing. `yad connect` does not reach a
  running daemon, and the connection it adds is synced by the next start —
  which may be the release this one takes over as — so the file is the right
  source and not the daemon's own list. A self-update reads its own
  profile's record as its own needs for the same reason.
- **The vet reads every record before the download.** A release missing a
  major any live record names is refused, and the refusal names the profile,
  its connections and its config directory. That is local only — the log and
  `yad status`; self-update refusals do not reach a hub — so a path under the
  owner's home is fine there. Its shape is 0071's: the majors, who needs
  them, and `yad upgrade --tag` to force it.
- **A refused release is judged again at each check, without a download**:
  what it said of itself is kept, and once nothing needs more than it speaks
  — another profile let its hub go — it is downloaded and taken. 0071 made a
  refusal last for the life of the process, which was right while the only
  needs were the process's own; a release that misnames itself is still
  refused for good.
- **A record whose profile is gone** — its config directory holds no
  `config.toml` — is skipped and removed by the updater that sees it. A
  config directory the updater cannot even look into counts as there.
- **A record that cannot be read fails the check** rather than refusing the
  release: nothing is downloaded, the error names the file and gives the
  `rm` that removes it, and the next check goes ahead once it is gone or its
  daemon has written it again. A refusal would last until the next release,
  for a file a person can delete in a second.
- **A daemon that cannot write its record carries on.** It warns once in the
  log, and `yad status` says so while it lasts; it tries again every minute.
  Absence is data, never a reason not to run.

## Consequences

A binary in a directory its runner's user cannot write — root-owned
`/usr/local/bin`, as the kit installs it with self-update off — leaves that
profile unrecorded, with the warning. Only a user who can write there could
update the binary, and 0071 keeps self-update to binaries the runner's user
owns. Releases older than this one write no record, so a profile still on
one is invisible until it upgrades; with only v1 released, that costs
nothing.

## Considered options

**A registry of profiles in the owner's home**, such as
`~/.config/yad/profiles.d`: a profile that moved its config directory with
`YAD_CONFIG_DIR`, or one run by another user of the same binary, would not
be there, and the binary is what the vet is about.
**Enumerating profiles by walking the default directories**: misses every
profile kept elsewhere, which is the case that made 0071 give up.
**A refusal that lasts, as 0071's did**: correct for the process's own
connections, which change only with a restart, and wrong for another
profile's, which change while this one runs.
