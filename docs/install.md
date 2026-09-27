# Installing, upgrading and running YAD

## From a release

```bash
curl -fsSL https://raw.githubusercontent.com/skkap/yad/master/scripts/install.sh -o yad-install.sh &&
  sh yad-install.sh
```

The `&&` is the point, not the two steps. A pipeline reports only its last
command's status, so `curl … | sh` with a `curl` that cannot fetch the script
hands `sh` an empty stream and exits 0 having installed nothing. Joined with
`&&`, a failed fetch fails the whole command. To read the script before it
runs, run the two halves separately.

It needs no login and no token: it reads the newest release from GitHub, puts
`yad` in `~/.local/bin`, checks the release's SHA-256 before writing anything,
and tells you if that directory is not on your `PATH`. Then `yad doctor`, which
shows which harnesses it can drive. A harness has to be installed and logged in
on its own first — `claude`, `codex` — because YAD uses the harness's own login
and keeps no token of its own.

## From source

You need Go 1.27.

```bash
git clone https://github.com/skkap/yad && cd yad
mkdir -p ~/.local/bin
make install        # builds ./bin/yad and copies it to ~/.local/bin/yad
yad doctor
```

If `yad` is not found afterwards, `~/.local/bin` is not on your `PATH`.

## Pinning, upgrading, forks

`YAD_VERSION` pins a release and `YAD_INSTALL_DIR` moves where it lands:

```bash
YAD_VERSION=v0.3.1 sh yad-install.sh   # the file the command above left behind
```

Later, on your command and never on its own:

```bash
yad upgrade --check       # what the newest release is; changes nothing
yad upgrade               # fetch it, verify its checksum, then replace this binary
yad upgrade --tag v0.3.1  # that release, newer or older — how a bad one is rolled back
```

If you installed from a fork, set `YAD_REPO` for the upgrade too — nothing
records where the binary came from, so an upgrade without it would replace your
fork's build with upstream's.

`yad upgrade` downloads to a temporary directory beside the installed binary,
checks its SHA-256 against the release's `checksums.txt`, and only then renames
it into place — so an upgrade that fails at any step leaves a working `yad`. It
restarts nothing: a runner already running holds the binary it started from
until you restart it, and `yad upgrade` says so — naming the profile, and
carrying it in the commands it offers. If that runner is a service, re-run
`yad service install [--profile name]` rather than `yad daemon restart`:
install replaces the unit and starts it again, where a restart would leave an
unsupervised process the service manager is no longer watching
([0028](decisions/0028-a-runner-is-a-per-user-service-with-its-login-path.md)).
The profile goes after `service install` but *before* `daemon` —
`yad --profile work daemon restart` — because `daemon` has no flag of its own. Nothing in
YAD updates itself on a schedule or on a hub's say-so
([0018](decisions/0018-no-self-update-in-v1.md)).

Releases are built by CI on a `v*` tag: linux and darwin × amd64 and arm64,
`CGO_ENABLED=0`, with a `checksums.txt` covering all four. To check a download
by hand, pull out the one line for the binary you took — a checker given the
whole file reports the three you did not download as failures:

```bash
grep " yad-linux-amd64$" checksums.txt | sha256sum -c -   # or: shasum -a 256 -c -
```

## Run it as a service

```bash
yad service install [--profile name]     # launchd agent on macOS, systemd user unit on Linux
yad service status  [--profile name]
yad service uninstall [--profile name]   # stops it and removes the unit; safe to repeat
```

The service runs `yad daemon start --foreground` as you, never as root, and
restarts it after a crash. It uses the PATH your login shell had when you ran
`install`, so run `install` again after changing PATH, upgrading or moving the
binary; a re-install replaces the unit. Stopping the service drains the
runner — no new runs, the ones it holds finish for up to `[drain] wait`
(default 30m), then are cancelled — and the unit's stop timeout is derived
from that wait when you install, so run `install` again after changing it. On Linux a user unit stops when you log
out unless lingering is on — `install` tells you, and `loginctl enable-linger`
is yours to run. Why it is shaped this way: [0028](decisions/0028-a-runner-is-a-per-user-service-with-its-login-path.md).
