-- Every hub-issued id (session, run) is addressed together with its
-- connection; see the note at the top of migrations/0001_init.sql.

-- ASCII only in this file, comments included. sqlc 1.31.1 rewrites each query
-- using byte offsets it computed over runes, so one multi-byte character
-- anywhere above a query truncates that query's text and every later one:
-- `SELECT *` arrives at its parser as `SELECharness` and generation fails
-- naming queries nobody touched. The repo's prose uses an em dash; here it
-- costs an afternoon. (Confirmed by adding one and removing it again, DEV-27.)

-- name: CreateSession :exec
INSERT INTO sessions (connection, id, harness, account, workdir, created_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetSession :one
SELECT * FROM sessions WHERE connection = ? AND id = ?;

-- name: SetSessionNativeID :exec
UPDATE sessions SET native_id = ?, last_used_at = ? WHERE connection = ? AND id = ?;

-- name: SetSessionWorkdir :exec
UPDATE sessions SET workdir = ?, last_used_at = ? WHERE connection = ? AND id = ?;

-- name: SetSessionState :exec
UPDATE sessions SET state = ?, last_used_at = ? WHERE connection = ? AND id = ?;

-- Workdir collection (decision 0035). A run held (claimed, preparing,
-- running, or waiting on a usage limit or its start time) keeps its session
-- open whatever else is true: its workdir is in use. The close is one
-- statement, so a claim cannot slip a run into the session between the check
-- and the write; it answers 0 rows when a run is held or the session is not
-- open. A reason already asked for while a run was held stands.
-- name: CloseSession :execrows
UPDATE sessions SET state = sqlc.arg(state), close_reason = COALESCE(close_reason, sqlc.arg(reason)), closed_at = sqlc.arg(now),
  close_requested_at = NULL
WHERE sessions.connection = sqlc.arg(connection) AND sessions.id = sqlc.arg(id) AND sessions.state = 'open'
  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.connection = sessions.connection AND r.session_id = sessions.id
    AND r.state IN ('claimed', 'preparing', 'running', 'waiting'));

-- The first reason asked for stands: a hub's close repeated on every sync
-- must not turn the owner's into the hub's.
-- name: RequestSessionClose :exec
UPDATE sessions SET close_requested_at = COALESCE(close_requested_at, sqlc.arg(now)),
  close_reason = COALESCE(close_reason, sqlc.arg(reason))
WHERE connection = sqlc.arg(connection) AND id = sqlc.arg(id) AND state = 'open';

-- name: HeldRunInSession :one
SELECT CAST(COALESCE((SELECT r.id FROM runs r WHERE r.connection = sqlc.arg(connection) AND r.session_id = sqlc.arg(id)
  AND r.state IN ('claimed', 'preparing', 'running', 'waiting')), '') AS TEXT);

-- name: SessionsCloseRequested :many
SELECT * FROM sessions s WHERE s.state = 'open' AND s.close_requested_at IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.connection = s.connection AND r.session_id = s.id
    AND r.state IN ('claimed', 'preparing', 'running', 'waiting'))
ORDER BY s.close_requested_at;

-- A last-used stamp that is missing is unknown, never ancient: the idle TTL
-- starts from when collection first saw the session. Multica's collector
-- read a missing stamp as the epoch and reclaimed everything at once.
-- name: StampUnknownLastUsed :execrows
UPDATE sessions SET last_used_at = sqlc.arg(now) WHERE state = 'open' AND last_used_at <= 0;

-- Open sessions nothing has run in since before idle_since, longest idle
-- first: the idle TTL's and disk pressure's candidates. Disk pressure asks
-- only for sessions with a workdir, since the rest free nothing and would
-- fill its page.
-- name: IdleSessions :many
SELECT * FROM sessions s WHERE s.state = 'open' AND s.last_used_at > 0 AND s.last_used_at < sqlc.arg(idle_since)
  AND (sqlc.arg(with_workdir) = 0 OR s.workdir != '')
  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.connection = s.connection AND r.session_id = s.id
    AND r.state IN ('claimed', 'preparing', 'running', 'waiting'))
ORDER BY s.last_used_at, s.connection, s.id
LIMIT sqlc.arg(max);

-- name: UnreclaimedSessions :many
SELECT * FROM sessions WHERE state != 'open' AND reclaimed_at IS NULL ORDER BY closed_at;

-- name: SetSessionReclaimed :exec
UPDATE sessions SET reclaimed_at = ? WHERE connection = ? AND id = ?;

-- name: UnreportedClosedSessions :many
SELECT * FROM sessions WHERE connection = sqlc.arg(connection) AND state != 'open' AND reported_at IS NULL
ORDER BY closed_at, id LIMIT sqlc.arg(max);

-- name: SetSessionReported :exec
UPDATE sessions SET reported_at = ? WHERE connection = ? AND id = ? AND reported_at IS NULL;

-- name: CreateRun :exec
INSERT INTO runs (connection, id, session_id, harness, model, state, spec, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'claimed', ?, ?, ?);

-- name: GetRun :one
SELECT * FROM runs WHERE connection = ? AND id = ?;

-- name: SetRunState :exec
UPDATE runs SET state = ?, resumes_at = ?, reason = ?, updated_at = ? WHERE connection = ? AND id = ?;

-- name: SetRunAccount :exec
UPDATE runs SET account = ?, updated_at = ? WHERE connection = ? AND id = ?;

-- name: ListHeldRuns :many
SELECT * FROM runs
WHERE connection = ? AND state IN ('claimed', 'preparing', 'running', 'waiting')
ORDER BY created_at;

-- name: AppendEvent :exec
INSERT INTO events (connection, run_id, seq, body) VALUES (?, ?, ?, ?);

-- name: UnackedEvents :many
SELECT seq, body FROM events WHERE connection = ? AND run_id = ? AND acked = 0 ORDER BY seq LIMIT ?;

-- The hub's acked_through is authoritative both ways: everything after it is
-- unacknowledged again, so a hub that lost events gets them resent.
-- name: AckEvents :exec
UPDATE events SET acked = (seq <= sqlc.arg(acked_through))
WHERE connection = sqlc.arg(connection) AND run_id = sqlc.arg(run_id);

-- name: DropEvents :exec
UPDATE events SET acked = 1 WHERE connection = ? AND run_id = ?;

-- name: RunsWithUnackedEvents :many
SELECT DISTINCT run_id FROM events WHERE connection = ? AND acked = 0 ORDER BY run_id;

-- name: HasUnackedEvents :one
SELECT EXISTS (SELECT 1 FROM events WHERE connection = ? AND run_id = ? AND acked = 0);

-- name: SpoolDepth :one
SELECT count(*) FROM events WHERE acked = 0;

-- name: PutOutbox :exec
INSERT INTO outbox (connection, run_id, body, next_attempt_at) VALUES (?, ?, ?, ?)
ON CONFLICT (connection, run_id) DO NOTHING;

-- name: DueOutbox :many
SELECT * FROM outbox WHERE connection = ? AND next_attempt_at <= ? ORDER BY next_attempt_at;

-- name: RetryOutbox :exec
UPDATE outbox SET attempts = attempts + 1, next_attempt_at = ?, last_error = ? WHERE connection = ? AND run_id = ?;

-- name: DeleteOutbox :exec
DELETE FROM outbox WHERE connection = ? AND run_id = ?;

-- A finished run whose result the hub has not acknowledged is still this
-- runner's: listing it keeps its lease alive, so a hub outage longer than a
-- lease does not turn a finished run into a lost one.
-- name: ListReportingRuns :many
SELECT r.* FROM runs r JOIN outbox o ON o.connection = r.connection AND o.run_id = r.id
WHERE r.connection = ? ORDER BY r.created_at;

-- name: OutboxDepth :one
SELECT count(*) FROM outbox;

-- A usage limit: the account is at one until its window resets. The state and
-- the reset are written together because they are one fact: a limited account
-- with no reset is a park nothing ends, and a reset with no state is a limit
-- nobody acts on.
-- name: SetAccountLimit :exec
INSERT INTO accounts (harness, label, state, limited_until, updated_at) VALUES (?, ?, 'limited', ?, ?)
ON CONFLICT (harness, label) DO UPDATE SET state = excluded.state, limited_until = excluded.limited_until, updated_at = excluded.updated_at;

-- One window's latest use and reset. An upsert per window rather than a
-- rewrite of a whole snapshot, so two turns finishing on the same account at
-- once cannot lose each other's windows: capacity is a shared pool and nothing
-- reserves an account.
--
-- The row keeps the most recently *recorded* snapshot, not the most recently
-- observed one. updated_at is when the turn ended, which is the only time any
-- caller has: a window carries its use and its reset and never the moment the
-- harness said them. So this resolves two turns racing to write - the one that
-- started earlier and arrived later no longer wins - and does not resolve two
-- turns that observed at different moments, since a long turn can observe
-- early and still end last.
--
-- One consequence worth knowing before changing it: the comparison is against
-- a wall clock, and :exec discards the row count. A clock stepped backwards
-- silently skips every window write for an account written just before the
-- step, until the clock passes the stored stamp. That is confined to what a
-- hub is shown - SetAccountLimit carries no such WHERE, so parking an account
-- still works, and RefillAt ignores a reset that has already passed.
-- name: SetAccountWindow :exec
INSERT INTO account_windows (harness, label, name, used_percent, resets_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (harness, label, name) DO UPDATE SET used_percent = excluded.used_percent, resets_at = excluded.resets_at, updated_at = excluded.updated_at
WHERE excluded.updated_at >= account_windows.updated_at;

-- name: ListAccounts :many
SELECT * FROM accounts WHERE harness = ? ORDER BY label;

-- The state an account is in, without disturbing limited_until: DEV-27 owns
-- the limit, this owns free and needs_login.
-- name: SetAccountState :exec
INSERT INTO accounts (harness, label, state, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT (harness, label) DO UPDATE SET state = excluded.state, updated_at = excluded.updated_at;

-- Every account this runner has a row for, across harnesses: what
-- `yad account list` and the health report read. The owner's order lives in
-- config.toml, so this sorts only for a stable read.
-- name: ListAllAccounts :many
SELECT * FROM accounts ORDER BY harness, label;

-- Every window this runner has heard of, across accounts: what the health
-- report and `yad account list` read, grouped by account in Go.
-- name: ListAllAccountWindows :many
SELECT * FROM account_windows ORDER BY harness, label, name;

-- name: DeleteAccount :exec
DELETE FROM accounts WHERE harness = ? AND label = ?;

-- An account's windows go with the account. Left behind, they would be
-- reported against a label the owner re-added for a different subscription.
-- name: DeleteAccountWindows :exec
DELETE FROM account_windows WHERE harness = ? AND label = ?;

-- name: TakeSlot :exec
INSERT INTO slots (repo, slot, connection, session_id) VALUES (?, ?, ?, ?);

-- name: SessionSlot :one
SELECT slot FROM slots WHERE repo = ? AND connection = ? AND session_id = ?;

-- name: SlotsInUse :many
SELECT slot FROM slots WHERE repo = ? ORDER BY slot;

-- name: FreeSlots :exec
DELETE FROM slots WHERE connection = ? AND session_id = ?;

-- A claim the hub withdrew before it started left nothing worth keeping, and
-- the hub may offer the same run again; the row must not be in the way.
-- name: DeleteUnstartedRun :exec
DELETE FROM runs WHERE runs.connection = sqlc.arg(connection) AND runs.id = sqlc.arg(id)
  AND NOT EXISTS (SELECT 1 FROM events e WHERE e.connection = runs.connection AND e.run_id = runs.id)
  AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.connection = runs.connection AND o.run_id = runs.id);

-- name: DeleteEmptySession :exec
DELETE FROM sessions WHERE sessions.connection = sqlc.arg(connection) AND sessions.id = sqlc.arg(id)
  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.connection = sessions.connection AND r.session_id = sessions.id);

-- The last event a run spooled, acknowledged or not: a result's last_seq.
-- name: LastEventSeq :one
SELECT CAST(COALESCE(MAX(seq), 0) AS INTEGER) FROM events WHERE connection = ? AND run_id = ?;

-- What `yad status` lists: every run held, across connections.
-- name: ListAllHeldRuns :many
SELECT * FROM runs
WHERE state IN ('claimed', 'preparing', 'running', 'waiting')
ORDER BY created_at;

-- name: CountOpenSessions :one
SELECT count(*) FROM sessions WHERE state = 'open';

-- A session's idle time runs from the end of its last run, not its start: a
-- run of three hours leaves a session used three hours later than it began.
-- name: TouchSession :exec
UPDATE sessions SET last_used_at = ? WHERE connection = ? AND id = ?;

-- What `yad sessions` lists: every session, most recently used first, with
-- the run live in it, if any.
-- name: ListSessions :many
SELECT s.connection, s.id, s.harness, s.native_id, s.workdir, s.state, s.created_at, s.last_used_at,
  s.close_reason, s.close_requested_at, s.closed_at, s.reclaimed_at, s.reported_at,
  CAST(COALESCE((SELECT r.id FROM runs r
    WHERE r.connection = s.connection AND r.session_id = s.id
      AND r.state IN ('claimed', 'preparing', 'running', 'waiting')), '') AS TEXT) AS live_run,
  CAST((SELECT count(*) FROM runs r WHERE r.connection = s.connection AND r.session_id = s.id) AS INTEGER) AS runs
FROM sessions s
ORDER BY s.last_used_at DESC, s.connection, s.id;

-- name: SetSessionSources :exec
UPDATE sessions SET sources = ? WHERE connection = ? AND id = ?;
