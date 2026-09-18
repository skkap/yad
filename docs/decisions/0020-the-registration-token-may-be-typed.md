---
date: 2026-09-19
---

# The registration token may be typed on the command line; stdin is preferred

`yad connect <url> --token T` is the shape every runner registration shares
(GitHub, Buildkite, GitLab), and it is what an owner copies from a hub's "Add
runner" page. It also puts a secret in argv, which the guardrails forbid: argv
is visible to `ps` and lands in shell history.

The registration token is the one exception, for three reasons together: it is
single-use and dead the moment the exchange succeeds, which is within a second
of the command starting; it expires on its own within an hour by default
(`yad hub token create --ttl`, at most a week); and it grants only the right to
register a runner, never access to any run. What it can leak is one
registration, briefly — and the hub records which runner used it.

`--token -` reads it from stdin instead, and is what `yad hub token create`
suggests. Nothing else — the runner credential, grants — is ever accepted in
argv, and `yad connect` never prints either.

## Considered options

**Stdin or a file only.** Keeps the rule without an exception, and breaks the
one-line command every hub's "Add runner" page will show. **An environment
variable** — no better than argv for a token typed on a command line, and
inherited by every child.
