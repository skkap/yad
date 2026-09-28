# Security

YAD runs on machines that hold tokens and runs harnesses with filesystem
access, so a vulnerability here lands on someone's own machine. Reports are
welcome and are handled before anything else.

## Reporting a vulnerability

Report it privately, through
[GitHub's private vulnerability reporting](https://github.com/skkap/yad/security/advisories/new).
Please do not open a public issue, and never include a live token — a made-up
one shows the problem just as well.

Include what you ran, what happened, and the version (`yad version`). A
maintainer aims to acknowledge a report within a week and to agree a
disclosure date with you once a fix is ready. YAD has one maintainer, so please
allow for that.

Only the newest release is supported: a fix ships as a new release, and
`yad upgrade` installs it.

## What counts

These break a promise YAD makes, and are vulnerabilities:

- A token, credential or grant that is printed, logged, sent in an event,
  passed in argv, or written anywhere other than a `0600` file.
- A hub that can set or widen what a harness may do — its permission mode,
  sandbox or caps — which YAD keeps as the owner's configuration.
- A hub that adds or removes an account on a connection whose owner set
  `manage_accounts = false`, or that adds one without its harness's own login
  check saying yes
  ([0057](docs/decisions/0057-a-hub-may-add-and-remove-accounts-unless-the-owner-says-no.md)).
- A grant that reaches the prompt, the logs, the events or the checkout, or
  outlives its run.
- A source that reaches a directory outside the owner's `[workdirs] roots`, or
  any directory on a runner whose owner set `path_sources = false`.
- The runner listening on a network port. Its only socket is a Unix socket for
  its own CLI.
- `yad upgrade` or `scripts/install.sh` installing a binary whose checksum does
  not match its release.
- `yad hub` accepting a call it should refuse: a runner acting on another
  runner's run, or a request without a valid token.

## What does not count, by design

- **What a connected hub asks for.** A hub writes the brief, and a brief can
  ask the harness for anything the machine allows. You choose the hubs you
  connect, and YAD does not police what they send
  ([0038](docs/decisions/0038-the-owner-trusts-the-hubs-it-connects.md)).
- **What a harness does on the machine.** YAD is not a sandbox: a run can do
  anything the user it runs as can do. Confining it is the machine's job — a
  VM, a container, a dedicated OS user. [docs/run-it-safely.md](docs/run-it-safely.md)
  says how.
- Vulnerabilities in the harnesses themselves (Claude Code, Codex). Report
  those to their vendors.
