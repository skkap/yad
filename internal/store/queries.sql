-- Every hub-issued id (session, run) is addressed together with its
-- connection; see the note at the top of migrations/0001_init.sql.

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

-- name: SetAccountLimit :exec
INSERT INTO accounts (harness, label, limited_until) VALUES (?, ?, ?)
ON CONFLICT (harness, label) DO UPDATE SET limited_until = excluded.limited_until;

-- name: ListAccounts :many
SELECT * FROM accounts WHERE harness = ? ORDER BY label;

-- name: TakeSlot :exec
INSERT INTO slots (repo, slot, connection, session_id) VALUES (?, ?, ?, ?);

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
