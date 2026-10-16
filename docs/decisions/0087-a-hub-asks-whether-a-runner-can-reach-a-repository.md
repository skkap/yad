---
date: 2026-10-16
status: accepted
---

# A hub asks whether a runner can reach a repository

A hub cannot tell whether a runner can pull a repository. `host_tools` says
the runner's `gh` is logged in to `github.com`, but repositories in different
organisations need different accounts, and a token expires without a word: on
2026-10-16 Ashikaga's `gh` token was invalid, and nothing showed it until a
run failed `source_failed` in its preparation. Zumino's outsourcing sends
work for several organisations' repositories to whichever runner is free, so
a runner that cannot fetch one is a run lost every time it is picked. YAD-200.

## Decision

The owner decided on ZUM-128 (2026-10-16): **the hub asks, the runner checks
with its own credentials, and the hub routes on the answer.** Picked over
learning from failed clones, which spends a run to find out and says nothing
after the credential is fixed, and over the owner declaring which
repositories a machine can reach, which is exactly the list that goes stale
when a token expires.

**On the wire, behind a feature, `source_probe`.**

```
← probe_sources: [{ url, ref? }]                        // sync answer
→ source_reach:  [{ url, reachable, checked_at, error? }] // later sync request
```

- **The hub's list is the whole list in every answer.** A URL left out is
  no longer asked about; absent asks about nothing. Stating it whole makes a
  lost answer harmless and leaves nothing to acknowledge, as `drain` and
  `close_session` need; it costs a few hundred bytes a sync. At most 64
  distinct URLs count — the cap is the runner's, so a hub listing its whole
  history cannot have it start a thousand `git` processes.
- **The answer is sent on change, and again at least every 30 minutes.** A
  hub that lost what it held — a restore, a new hub behind the same URL — is
  whole again within that. An answer the hub did not take with a 2xx is sent
  again, as every other report is.
- **`error` is a class**, a closed enum: `auth`, `not_found`,
  `ref_not_found`, `network`, `timeout`, `refused`, `failed`. Never `git`'s
  own words: they can quote a URL into which the owner's `insteadOf` rule put
  a token, and a hub logs what it is sent. `ref_not_found` is the
  repository's answer, not the runner's — every runner would say it — so it
  is the one class a hub does not route away on; `RoutesAway` says so in
  code.
- An old runner drops `probe_sources` and never answers; a hub gating on the
  feature never sends it one. A hub that never probes still takes a sync
  carrying `source_reach` — a feature gates what is sent, never what is
  taken ([0048](0048-six-hub-behaviours-settled-by-zuminos-hub.md)) — and
  conformance checks both halves.

**The check is a run's own fetch.** `git ls-remote --exit-code -- <url>
<ref|HEAD>`, through `workdir.Manager.Probe`: the same `git` host-tool
detection picks, the owner's `[env]` layer
([0081](0081-the-owner-declares-an-environment-for-everything-yad-starts.md)),
the credential helpers and ssh agent the daemon has, no prompt, and the URL
through the same `parseRemote` a run's sources go through — so a remote
helper, plain http, or a path outside the roots is refused unprobed, and the
answer is about the fetch a run would make. `ls-remote` rather than a fetch
because it reads the refs and nothing else: no cache to build, no disk. Each
check is stopped at **20 s**, which a reachable remote never comes near.

**A URL carrying a credential is refused, never probed.** Such a URL is a
run's own way in ([0068](0068-a-credential-in-a-source-url-is-the-runs-alone.md)):
probing it would say only whether the hub's token works, which the hub can
learn itself, and would have the runner hold a hub's token past any run. It
is answered `refused`, and `yad hub` never lists one.

**Cached per repository and ref, shared by every hub.** The answer is the
machine's, whichever hub asked. Checked at once when a list first names it,
then again every **30 minutes** while reachable and every **5** while not —
shorter, because what follows an unreachable answer is usually its owner
fixing the credential, and the hub waiting to route back. Four checks at
most run at once. This is the runner's second timer of its own besides its
syncs and sweeps, after the self-update check; it runs only while a hub
lists something.

**Routing.** A run opening a session with a `git` source a runner last said
it cannot reach, for any class but `ref_not_found`, goes to another runner.
No answer yet is no reason to hold a run back: the run's own preparation
remains the last word. A run in a session bound to the runner goes
regardless — its workdir is built and it can go nowhere else. A run every
runner says it cannot reach stays queued until one can.

**`yad hub`** lists the distinct git URLs of the runs submitted in the last
seven days, the most recently used first, without those carrying a
credential; keeps each runner's latest answer per URL in
`runner_source_reach`, replacing it only with a newer `checked_at` and
deleting it once the URL is no longer listed; routes as above; and shows the
answers in `GET /runners` and `yad hub runners`. `yad status` shows each
connection's list and answers on the runner.

## Consequences

- A hub sees a broken credential within five minutes of asking and a fixed
  one within five minutes of the fix, without spending a run.
- Every runner advertising `source_probe` runs up to 64 `ls-remote`s per
  half hour per hub list — a few kilobytes against each remote.
- An unreachable answer is logged once per change as a warning, with the URL
  redacted as a hub URL is, so it shows in `yad status`'s recent errors and
  in the health the hub is sent.
- `yad conformance` checks that a hub sends no list to a runner not
  advertising `source_probe` and takes a sync carrying `source_reach`; what
  a hub does with the answers is on the not-checked list, since seeing a run
  routed needs runs only the hub's own queueing can make.
