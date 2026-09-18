---
date: 2026-09-19
---

# yad hub's service API sits beside the protocol, with its own token and document, and streams by long poll

`yad hub` needs a way for a service (yashiki first) and a person to put a run
in and follow it. That surface is not the runner protocol: Zumino and yashiki,
when they embed the protocol, make runs their own way and must never have to
implement this. So it is kept apart on every axis a hub implementer would look
at.

- **Its own base path.** `/api/v1`, beside the protocol's `/v1`. The major
  version is in the path, as the protocol's is, and moves on its own.
- **Its own types and generated document.** `protocol/hubapi` holds the Go
  types — reusing `protocol/v1`'s brief, sources, grants, event and result, so a
  caller that knows one knows both — and `protocol/hubapi/openapi.yaml` is
  generated from the operations and committed, with the same drift check as the
  protocol's ([0017](0017-protocol-types-are-the-source.md)). A TypeScript
  service generates its client from that file alone, and a hub reading
  `protocol/v1/openapi.yaml` never sees these operations.
- **Its own token.** An **admin token** (`yadadm_…`), created on the hub's
  machine with `yad hub admin-token create`, stored by the hub as a SHA-256 hash
  in its own table, named so it can be listed and revoked. The service API
  accepts only admin tokens and the protocol never does; a runner credential is
  refused on `/api/v1` — a runner cannot submit work, even to itself.
- **Streaming is a long poll with a sequence cursor.**
  `GET /runs/{run}/events?after=N&wait_ms=…` answers at once with the events
  after `N`, or when the run's state moves, or holds the request up to 50 s and
  answers empty. The caller asks again with `after = next_after` until `done`.
  `done` is the end of the *stream*, not of the run: the runner's result names
  its last event, and may arrive before its final batch, so a page is done only
  once the run is terminal and every event up to that one has been returned.
- **Submit is idempotent by caller-chosen run id.** The same `run_id` with the
  same content answers the run already queued; with other content, 409. Omitted
  ids — run and session — are generated. A session is `new` or continued
  explicitly, and the hub checks it: a "new" run in an existing session would
  make the runner start the conversation over, a continuing run in no session
  would resume nothing.
- **The shape leaves room for cancel.** Runs are a resource (`/runs/{run}`), so
  cancel arrives as `POST /runs/{run}/cancel` (DEV-8) without touching anything
  here.

The CLI (`yad hub submit`, `yad hub watch`) finds the admin token in a `0600`
file — the profile's `hub-admin-token` by default, `--token-file` otherwise —
and never in argv or the environment. Decision
[0020](0020-the-registration-token-may-be-typed.md)'s exception does not extend
here: an admin token is long-lived and opens every run on the hub, where a
registration token is single-use and short-lived. `admin-token create` writes
the file and prints nothing secret; `--out -` prints the token once, to stdout
alone, for a service's secret store.

## Considered options

**Server-sent events.** The natural "stream" on paper. But the browser's and
Node's `EventSource` cannot send an `Authorization` header, so a TypeScript
service would need a third-party client or a token in the URL; SSE has no
place in an OpenAPI document that generators turn into types; and a dropped
connection still needs the same `after` cursor to resume. The long poll is one
typed JSON call every generated client already makes, resumes by construction,
passes any proxy, and costs at most one request per 50 s per watcher. SSE can
be added later as a second representation of the same cursor.

**One document for protocol and service API.** Fewer files, and every
protocol implementer would read, and probably generate, operations they must
not implement.

**Runner credentials or registration tokens for the API.** No new secret kind,
and a machine that can run work could also create it — the opposite of
"nothing a hub sends can widen what the owner configured", seen from the other
side.

**The admin token in an environment variable for the CLI.** Customary for
services, which read their own secret store and call the API directly; for the
CLI it is inherited by every child and no better than argv once typed.
