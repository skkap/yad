---
date: 2026-09-30
---

# A credential in a source's URL is the run's alone

Decision [0033](0033-sources-reach-only-what-the-owner-allows.md) refused a git
URL that carried a password, and let through one whose user was a token —
`https://ghp_xxx@github.com/org/repo`, the usual shape of a GitHub or GitLab
token. DEV-96 stopped that token being quoted back in refusals, events and
git's reasons, but it was still handed to git as argv, where every user on the
machine can read it, and written as `remote.origin.url` into the bare cache
under `<data>/repos`, which outlives the run and which every later run, from
any hub, reads. It sat in `state.db` too, in the run's spec and the session's
sources (DEV-144).

## Decision

The owner's (2026-09-30).

**An https URL's userinfo — a token as the user, or a user and a password — is
accepted, and treated as a credential for that run alone, like a grant
([0009](0009-machine-owns-credentials-hubs-grant-per-run.md)).** The hub is
trusted ([0038](0038-the-owner-trusts-the-hubs-it-connects.md)); what this
removes is persistence and one hub's credential being readable by the runs of
another.

- **Taken out before anything is stored or keyed.** The URL git is given, the
  cache's `remote.origin.url` and its directory name, the run's spec and the
  session's sources in `state.db`, the daemon's log and the run's events all
  hold the URL without its userinfo (`withoutCredential`, `StoredSources` in
  `internal/workdir/source.go`). Runs from one repository with and without a
  credential share one cache.
- **Handed to git only through the environment, only for the run's own
  commands that talk to the remote** — its fetch, and asking the remote for its
  default branch. git reads a credential helper from `GIT_CONFIG_COUNT` /
  `GIT_CONFIG_KEY_n` / `GIT_CONFIG_VALUE_n`, scoped to the remote's scheme, host
  and port, which prints the credential from two variables beside it. Nothing
  holds it in argv or in any file. The helper list for that host is emptied
  first, so a helper in the owner's config neither answers before it nor is
  asked to store what worked — a keychain would otherwise keep the hub's token
  for good. A redirect to another host is not handed it. The setup hook and the
  harness never see it.
- **Only the run that was sent it.** A continuing run compares its sources
  with the session's without the credential, so it may send another one, or
  none; a later run without one is fetched with the machine's own, and never
  inherits it. A run parked before its fetch and picked up by a later process
  has lost it, as it would have lost its grants: it is reported `lost` with
  `grants_lost`, for the hub to send again.
- **A user alone is tried as a token first, then as a name.** Nothing in
  `https://x@host/…` says whether `x` is a token or the account's name, which
  Azure DevOps and Bitbucket put in the clone URLs they hand out. It is
  offered as a token with an empty password; when the remote refuses that, the
  command runs once more with `credential.<scheme>://<host>.username` set to it
  — still in the environment — and the owner's own helpers answer for that
  name, as they did when git was given the URL whole. That second try keeps
  git's old exposure, narrowed to a remote that refused the user as a token:
  an owner's helper that answers with a password alone leaves the user as the
  name, and on success git asks every helper to store the pair. A user and a
  password are the hub's whole credential, and get no second try.
- **A credential git cannot carry is refused** — a control character, sent as
  `%0A`, would be read by git's credential protocol as a line of its own.
- **ssh is unchanged.** `git@host` is the login name, not a credential, and an
  ssh URL with a password is still refused: ssh takes none from a URL.

Needs git 2.31 or later, which reads config from the environment; an older git
ignores it and fetches with the machine's own credentials. A cache an earlier
version made from a URL with userinfo keeps that URL in its config, under
another directory name than the one used now, until it is deleted.

This replaces 0033's "a URL that carries a password is refused".

## Considered options

**Refuse any userinfo in an https URL**, as 0033 refused a password. Simple,
and the machine's own credentials already reach most repositories. But a hub
scoping a token to one repository for one run is the shape grants exist for,
and refusing it pushes that hub to put the token somewhere worse.
**An `http.<url>.extraHeader` carrying `Authorization: Basic`.** Also
environment-only, and sent up front without a 401. git adds extra headers to
every request of the command, including those after a redirect to another
host, and the owner's helpers would still be asked on a 401.
**A `GIT_ASKPASS` program.** 0033 empties it so nothing prompts; bringing one
back needs a program on disk, and git asks it only for a password, not a user.
