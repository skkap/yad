# Running YAD in a container

A container is a quick way to give a runner a machine of its own: one image
with yad and its harnesses, one volume that holds everything the runner keeps,
and nothing of the host unless you mount it. This page is a recipe that works
with Docker; any OCI runtime does the same with its own flags.

## A container or a VM

Both keep a run away from the rest of your computer, and both are what
[run-it-safely.md](run-it-safely.md) means by *a machine of its own*. They
differ in how far:

- **A VM** ([`machines/`](../machines/README.md)) has its own kernel, and the
  kit shuts the runner's user out of the host, the LAN and the tailnet. It is
  the stronger boundary, and the one to choose when a hub you connect is not
  fully yours.
- **A container** shares the host's kernel and, by default, reaches whatever
  the host's network reaches. It is lighter and quicker to throw away, and a
  good fit for a server that already runs containers, or a runner you rebuild
  often. Restrict its network yourself if it matters — a Docker network with
  no route to your LAN, or a firewall in front of it.

Either way, YAD does not sandbox the harness inside it: a run can use
everything the container holds — every login, every file in the volume.

## The image

```dockerfile
# yad and its harnesses in one image; the runner runs as an ordinary user.
FROM golang:1.27-bookworm AS yad
ARG YAD_REF=master
RUN git clone --branch "$YAD_REF" https://github.com/skkap/yad /src
WORKDIR /src
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/skkap/yad/internal/buildinfo.Version=$(git describe --tags --always) -X github.com/skkap/yad/internal/buildinfo.Commit=$(git rev-parse --short HEAD)" \
      -o /out/yad ./cmd/yad

FROM node:22-bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git openssh-client \
 && rm -rf /var/lib/apt/lists/*
# Codex is pinned to the app-server protocol this yad was built against;
# `yad doctor` warns when an installed codex differs.
RUN npm install -g @anthropic-ai/claude-code @openai/codex@0.147.0
COPY --from=yad /out/yad /usr/local/bin/yad
USER node
WORKDIR /home/node
CMD ["yad", "daemon", "start", "--foreground"]
```

```bash
docker build -t yad-runner .                            # master
docker build -t yad-runner --build-arg YAD_REF=v0.1.0 .  # a release tag
```

The build flags stamp the version, which `yad version`, a hub's minimum
version and `yad upgrade` all read; a plain `go install` would leave it as
`dev`. The runner runs as `node`, an ordinary user: Claude Code refuses to
skip its permission prompts as root.

Add whatever the work needs — languages, CLIs, a `.gitconfig` — to the image,
the same way a work machine's spec adds them in `provision.sh`.

## Connect it to a hub, once

Everything the runner keeps lives under its home: its identity, its
credential for each hub, its accounts and their tokens, its sessions and
workdirs, and the harnesses' own logins. One volume on `/home/node` keeps all
of it across restarts and image upgrades. **Treat that volume as a secret.**

To start the runner with settings of your own — the accounts it runs on, its
capacity, harness settings — put a `config.toml` in the volume first.
`yad connect` adds the hub to it and keeps the rest:

```bash
docker run -i --rm -v yad-home:/home/node yad-runner \
  sh -c 'mkdir -p ~/.config/yad && umask 077 && cat > ~/.config/yad/config.toml' < config.toml
```

Get a registration token from your hub, and connect with it on stdin:

```bash
docker run -i --rm -v yad-home:/home/node yad-runner \
  yad connect https://hub.example/yad/v1 --token - --name box < registration-token.txt
```

The hub must be `https`. yad sends a runner credential on every request, and
refuses plain `http` to anything but its own loopback.

## Run it

```bash
docker run -d --name yad --restart unless-stopped \
  -v yad-home:/home/node --stop-timeout 1800 yad-runner
```

`docker stop` sends one SIGTERM, and the runner drains: it takes no new run,
lets the ones it holds finish for up to `[drain] wait` (30 minutes by
default), then exits. `--stop-timeout 1800` gives it that long before Docker
kills it; with Docker's default of 10 seconds a run in flight is killed and
reported `lost` at the next start. Shorten `[drain] wait` in the runner's
`config.toml` if you would rather stop faster.

## Log the harnesses in

The container starts with no harness logged in, and `yad doctor` says so. Log
each account in once; the login lives in the volume.

- **Claude, with a token.** Run `claude setup-token` on any computer with a
  browser, then hand the token to the runner on stdin:

  ```bash
  docker exec -i yad yad account add claude main --token - < claude-token.txt
  ```

  The running daemon takes the account up at once, and yad warns a month
  before the token's year is up.
- **Codex, with a device code**, entered on any computer:

  ```bash
  docker exec -it yad yad account add codex main --device
  ```

- **From the hub**, with no shell into the container at all, if your hub
  supports it. The hub adds the account and logs it in, in one step: a link
  to follow and a code to paste back, or a `claude setup-token` token to
  paste. The runner lists it in the `config.toml` in the volume once the
  login takes, and the hub can log it in again, or remove it, later. How that
  works, how to stop a hub doing it (`manage_accounts = false`), and the
  `yad hub` commands, are in
  [machines/README.md](../machines/README.md#logging-in). For Codex the
  hub shows a link and a code to type there instead.

Then check:

```bash
docker exec yad yad doctor
docker exec yad yad account list
```

## Upgrade

Build the image again at the new `YAD_REF`, then replace the container with
the same volume:

```bash
docker build -t yad-runner --build-arg YAD_REF=v0.2.0 .
docker stop yad && docker rm yad
docker run -d --name yad --restart unless-stopped \
  -v yad-home:/home/node --stop-timeout 1800 yad-runner
```

The runner comes back with the same identity, so the hub sees the same runner.
`yad upgrade` does not apply here: the image owns the binary, so a new version
is a new image.

## What not to do

- **Don't mount the Docker socket**, your home directory or a source checkout
  from the host. A run can use anything the container can reach, and the
  socket is the host.
- **Don't share one volume between two runners.** A volume is one runner's
  identity; two containers on it are one runner claiming to be two.
- **Don't run it as root.** Beyond Claude's refusal, a root runner hands every
  run the whole container.
