---
date: 2026-09-19
---

# A run's sources reach only what the owner allows, through git that never prompts

A run's sources come from a hub, and a hub is untrusted input
([0015](0015-owner-environment-is-the-trust-boundary.md)). A git URL, a base,
a branch and a path are strings someone else chose, and each ends up in git's
argv or names a directory on the owner's machine. DEV-16 made them real: the
runner fetches git sources into a bare cache per repository and runs path
sources in place.

**Transports.** A git source is fetched over `https` or `ssh` (`ssh://` or
scp-like `user@host:path`), with the machine's own credentials — `gh`, an SSH
key, a credential helper
([0009](0009-machine-owns-credentials-hubs-grant-per-run.md)). Plain `http://`
and `git://` are refused: they carry no integrity, and a runner is about to
run what they deliver. A remote helper (`ext::`, `fd::`, any
`<transport>::<address>`) is refused: `ext::` runs a command, and any helper is
a program chosen by the hub. A URL that carries a password is refused rather
than stripped: the secret would land in git's argv, the cache's config and
every error message, and the machine's own credentials are what reach a
repository. git enforces the same list itself (`GIT_ALLOW_PROTOCOL=https:ssh:file`),
so a redirect or a submodule cannot reach another.

**Local sources only inside roots.** A `path` source, and a git source whose
URL is a local path or `file://`, is taken only when it resolves — symlinks and
`..` included, after resolution — to a directory inside one of the owner's
`[workdirs] roots` in `config.toml`. With no roots configured, none is taken:
the default is that a hub reaches nothing on the machine but what it clones.
`file://` on another host is refused. The check covers local git URLs as well
as paths because a clone of `~/.ssh` or of another customer's checkout is a
read of it.

**No option injection.** Every hub string is given to git as argv, never to a
shell. A URL, base or branch beginning with `-` is refused, as is whitespace or
a control character anywhere; a host or user beginning with `-` is refused
(the `-oProxyCommand` class of bug). Refs follow git's own rules
(`check-ref-format`) plus the ones it leaves to callers: not `HEAD`, not `@`.
Positional arguments follow `--` or `--end-of-options`.

**git never prompts.** Every git command runs through the supervisor with the
owner's `[workdirs] git_timeout` (default 10 min), in a session of its own with
no controlling terminal — ssh prompts through `/dev/tty` whatever its
environment says — and with `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS` and
`SSH_ASKPASS` emptied, `SSH_ASKPASS_REQUIRE=never` and `GCM_INTERACTIVE=never`.
A repository that needs a password fails at once with `source_failed`, never
waits for a person who is not there.

**The cache.** One bare repository per URL under `<data>/repos/`, made with
`init --bare` and an origin whose refspec maps the remote's branches to
`refs/remotes/origin/*` — not `clone --bare`, which maps them onto its own
branches, where a fetch would move a branch a session has checked out. The
remote's tags are copied, forced and pruned, into `refs/yad/origin-tags/`,
which only resolves a base — a tag the remote moved or deleted is moved or
deleted there, and one no branch reaches is there too — while `refs/tags`,
which every worktree of the cache shares and a harness may tag its work in, is
left to git's own following, which never forces or prunes. `origin/HEAD`
follows the remote's default branch on every fetch (`followRemoteHEAD`, git
2.48+). A
first fetch is built aside and renamed in, so a failure leaves nothing half
made. Every change to a cache — a fetch, a worktree added or removed — holds
that repository's lock, since git's own lockfiles fail rather than wait; the
setup hook and the run do not.

**Branches.** The run's branch is checked out as it stands when the cache
already has it, tracked from the remote when the remote has it, and otherwise
cut from `base` — a remote branch, a tag or a commit; the remote's default
branch when none is named. A run that names no branch works on
`yad/<connection>/<session>`. A session that continues finds its worktree as it left it —
the uncommitted work in it is the session's — and nothing is fetched.
**A session keeps its sources.** The sources its first run named — none
included, and whether or not that run got as far as its harness — are recorded
with the session (`sessions.sources`), unless they were refused. A continuing
run that names none is prepared from them — a path source locked again, the
harness started where the conversation lives, a setup hook that failed run
again — and one naming others is refused with `source_refused`. A
worktree whose add never finished (a cancel or a timeout kills git, and git's
own cleanup does not run under SIGKILL) carries no completion mark in its git
directory, and is removed and made again rather than worked in.

**Layout.** One git source is checked out as the session's workdir itself, so
the harness starts at the repository's root and finds its `CLAUDE.md`. One
path source is used in place: the harness runs in that directory, which stays
locked until the run ends, so two runs never edit one directory at once. The
locks are `flock`s on the directories themselves — every profile on the
machine and every OS user contends for the same ones, and nothing is written
into the owner's tree — exclusive on the source and shared on every directory
above it, so a run on `/src` and one on `/src/app` wait for each other while
`/src/app` and `/src/lib` run side by side. A run's own path sources may not
nest, and each is resolved and checked against the roots again once its locks
are held, since the run holding a directory above it could have swapped it for
a link out of the roots while this one waited. Several sources lie side by side under the
workdir, a path source as a symlink to where it lives; one repository twice in
a run is refused, since both would take the session's one slot for it.

**Classes.** `source_refused` — the run breaks these rules; retrying it changes
nothing. `source_failed` — git could not fetch or check out: the network, the
machine's credentials, a branch another session holds. Both are additions to
the open set of error classes.

## Considered options

**`file://` refused outright.** Simplest, and tests would need a network
transport. Allowing it inside the roots costs one check and serves an owner who
keeps mirrors on disk.
**Roots default to the owner's home.** Convenient, and it hands every hub
`~/.ssh`. Refuse by default and make the owner name what runs may touch.
**Strip a password from a URL and go on.** The hub would learn nothing from the
refusal, and the owner's log would still have held it once.
