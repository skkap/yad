# Review profile — YAD

Every revmux round in this repository inherits this. It says what the software is,
what a failure costs here, and which of its habits are deliberate, so each finding
is measured against YAD's bar rather than a generic one.

## What it is

- **A daemon that runs coding-agent harnesses on machines people own.** A single
  Go 1.27 binary, `yad`. It detects Claude Code and Codex, connects outbound to one
  or more **hubs** (Zumino, yashiki, the built-in `yad hub`), claims **runs**,
  drives the harness in a workdir, streams events and reports a result. One
  maintainer.
- **It runs on machines holding tokens**, including customers' machines, and the
  harnesses it drives auto-approve everything they do (decision 0015). The owner's
  environment is the trust boundary, and YAD must never widen it.
- **The protocol (`protocol/v1`) is a public surface.** TypeScript hubs generate
  their types from `protocol/v1/openapi.yaml`, so a rename or removal breaks every
  one of them. The same applies to the service API (`protocol/hubapi`).
- **`yad hub`** is the same binary in server mode: SQLite, headless, and the
  reference implementation of the hub side.

## What a real failure looks like here

**A runner that silently does the wrong thing on someone else's machine is the
worst outcome.** Most of what matters is a variation on that.

- **A hub setting what the owner configures.** A protocol field, control or run
  spec that can set the permission mode, the sandbox, capacity, caps or accounts.
  The owner trusts its hubs (0038), so a hub may send any grant or brief, but
  the machine's settings are the owner's alone. Hub input is still data to YAD:
  never executed, never passed to a shell.
- **A token leaving the machine or landing on disk wrongly.** A token or grant in
  a log line, an event, argv, `yad harnesses` output or an error message. A
  credential file that is not `0600`.
- **A run executed twice, or reported with the wrong terminal state.** For
  example, a run started before its claim is acknowledged (0019), a late result
  overturning `lost` (0023), `succeeded` from an exit code rather than the
  harness's own result (0021), or events and results accepted from a runner that
  does not hold the run.
- **A process left behind.** A harness, git or probe child, or a setsid
  descendant, surviving its run, cancel or daemon stop. Or a pipe read with no
  bound that hangs a daemon tick.
- **Work lost across a crash or partition.** An event or result dropped rather
  than spooled or outboxed, a native session id written only at the end, or a
  drain that kills runs it promised to finish.
- **A protocol break.** A renamed or removed field, a changed meaning, a list
  sent as `null`, an unknown field refused, an error without the
  `{"error":{code,message,next_action}}` envelope, or `openapi.yaml` drifting from
  the Go types.
- **An opened network port.** The runner listens on nothing; its one socket is
  the `0600` Unix control socket (0004).
- **A migration that breaks an existing `state.db` or `hub.db`.** An edited
  shipped migration, a renumbering, or a number merged out of order: migrations
  apply by number against `PRAGMA user_version`, so a lower number merged after a
  higher one is silently skipped.

## Blast radius

- **Every machine running a runner**, including customers' machines, where a
  defect executes with the owner's credentials and filesystem.
- **Every hub** implementing the protocol from `openapi.yaml`.
- **Unattended runs**: nobody watches a runner, so a hang or a silent drop is
  noticed late, if ever.

## The reporting bar

Report what is **material**, not merely true.

- A deviation from a rule in `CLAUDE.md` (Guardrails), a `_Rules_` line in
  `DOMAIN.md`, or a decision record is always worth reporting.
- A concurrency, signal or process-lifetime bug is material even when the window
  is small. The code is built around supervised children and races with hubs.
- Prose caps at **minor**. The exceptions are `ARCHITECTURE.md` §2 (the protocol
  contract that hub implementers build against) and next-action text in errors,
  which operators follow literally.

**Noise here — do not report:**

- The length or style of comments. Comments argue *why* on purpose, and every
  non-obvious constant carries its reason.
- "Consider extracting / splitting / renaming" without a defect behind it.
- A missing assertion library, a missing mock framework, or a table test that
  could be "simpler". None of these is used, by rule.
- Suggestions to add a dependency. The dependency list is curated (0016), and the
  standard library is preferred.
- Tests that re-execute the test binary as a fake harness. That is the mechanism.

## Where the rules live

- `CLAUDE.md` holds the conventions and the **Guardrails**, and every guardrail
  is reportable when broken.
- `DOMAIN.md` owns the vocabulary. *Harness*, not agent; *hub*, not control plane;
  *run*, not task or job; *workdir*, not workspace; *capacity*, not slot (except
  `WT_SLOT`). Its `_Rules_` lines are invariants.
- `ARCHITECTURE.md` covers the shape, the v1 protocol (§2), how harnesses are
  driven (§3), local state (§4) and dependencies (§6).
- `docs/decisions/` holds forks already decided; do not re-argue them. Check
  there before calling a design choice a defect.
- `CHECKS.md` says what `make check` and CI run, and what they do not (`make
  smoke` spends real tokens and never runs in CI).
- Open work is in Zumino, project `yad/dev`
  (`zumino task list --open --project dev --workspace yad`). A defect already
  filed there is known, not new.

## Deliberate — do not file as defects

- **Runs auto-approve.** The default Claude permission mode is
  `bypassPermissions`, and YAD builds no sandbox or policy engine (0015). The
  permission mode is owner configuration, never a protocol field.
- **Pull only.** Runners sync every 10–60 s at the hub's direction, and a run can
  start up to one interval after submit (0005; DEV-53 is the known follow-up).
- **The registration token may be typed in argv** (0020); nothing else may.
- **The owner trusts the hubs it connects** (0038). Hub-supplied grant names and
  folder sources are not filtered for safety. That is by design, not a gap to
  report.
- **Lost is final**, even against a later `succeeded` from the runner (0023).
- **Multica is read for shapes, never code** (0014). A missing Multica feature is
  not a defect.
- **No self-update in v1** (0018).
- **Tests never spend a token or touch the network**. Loopback `httptest` and
  in-process hubs are the norm. Fixtures under `testdata/<harness>-<version>/`
  are recorded from real harnesses and replayed.

## Languages a change usually touches

- Go (the whole product and its tests, run with `-race`).
- SQL: `internal/store/migrations`, `internal/hub/store/migrations` (forward-only,
  numbered) and the sqlc queries. The Go under `db/` is generated.
- YAML: the generated `openapi.yaml` files and `.github/workflows/`.
- Bash: `scripts/smoke.sh`.
- A launchd plist and a systemd unit rendered by `internal/service`, with golden files
  in its `testdata/`.
- Markdown that people and agents act on: `ARCHITECTURE.md`, `DOMAIN.md`,
  decision records, `CHECKS.md`.
