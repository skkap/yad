-- Every hub-issued id (session, run) is addressed together with its
-- connection; see the note at the top of migrations/0001_init.sql.

-- name: CreateSession :exec
INSERT INTO sessions (connection, id, harness, account, workdir, created_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetSession :one
SELECT * FROM sessions WHERE connection = ? AND id = ?;

-- name: SetSessionNativeID :exec
UPDATE sessions SET native_id = ?, last_used_at = ? WHERE connection = ? AND id = ?;

-- name: SetSessionState :exec
UPDATE sessions SET state = ?, last_used_at = ? WHERE connection = ? AND id = ?;

-- name: ListIdleSessions :many
SELECT * FROM sessions WHERE state = 'open' AND last_used_at < ? ORDER BY last_used_at;

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

-- name: AckEvents :exec
UPDATE events SET acked = 1 WHERE connection = ? AND run_id = ? AND seq <= ?;

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
