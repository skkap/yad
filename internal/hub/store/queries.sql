-- name: CreateRegistrationToken :exec
INSERT INTO registration_tokens (hash, created_at, expires_at, for_runner) VALUES (?, ?, ?, ?);

-- name: GetRegistrationToken :one
SELECT * FROM registration_tokens WHERE hash = ?;

-- name: BurnRegistrationToken :execrows
-- Burning is one conditional update, so two exchanges racing on one token
-- cannot both see it unused.
UPDATE registration_tokens SET used_at = sqlc.arg(now), runner_id = sqlc.arg(runner_id)
WHERE hash = sqlc.arg(hash) AND used_at IS NULL AND expires_at > sqlc.arg(now);

-- name: UpsertRunner :exec
-- Registering an id that exists replaces its credential: the old one dies,
-- which is how a runner that lost its credential file is recovered. The
-- caller checks the token was issued for that id first.
INSERT INTO runners (id, name, credential_hash, capabilities, fingerprint, registered_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    name = excluded.name,
    credential_hash = excluded.credential_hash,
    capabilities = excluded.capabilities,
    fingerprint = excluded.fingerprint,
    wants_capabilities = 0,
    registered_at = excluded.registered_at;

-- name: GetRunner :one
SELECT * FROM runners WHERE id = ?;

-- name: GetRunnerByCredential :one
SELECT * FROM runners WHERE credential_hash = ?;

-- name: RecordSync :exec
UPDATE runners SET last_sync_at = ?, health = ?, wants_capabilities = ? WHERE id = ?;

-- name: SetCapabilities :exec
UPDATE runners SET capabilities = ?, fingerprint = ?, wants_capabilities = 0 WHERE id = ?;

-- name: CreateSession :exec
INSERT INTO sessions (id, harness, created_at) VALUES (?, ?, ?)
ON CONFLICT (id) DO NOTHING;

-- name: GetSession :one
SELECT * FROM sessions WHERE id = ?;

-- name: BindSession :exec
-- The first claim in a session binds it; later ones leave it as it is.
UPDATE sessions SET runner_id = ? WHERE id = ? AND runner_id IS NULL;

-- name: CreateRun :exec
INSERT INTO runs (id, session_id, harness, model, spec, state, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'queued', ?, ?);

-- name: GetRun :one
SELECT * FROM runs WHERE id = ?;

-- name: OfferCandidates :many
-- Queued runs this runner may be offered, oldest first, a page at a time
-- after the (created_at, id) cursor: only harnesses it can take now, only the
-- oldest queued run of each session, only in a session that is unbound or
-- bound to it, and never while another run of that session is out, since a
-- session has at most one live run. Filtering here rather than in Go is what
-- keeps runs it must skip from filling the page ahead of runs it could take.
SELECT r.* FROM runs r JOIN sessions s ON s.id = r.session_id
WHERE r.state = 'queued'
  AND r.harness IN (SELECT value FROM json_each(sqlc.arg(harnesses_json)))
  AND (r.created_at > sqlc.arg(after_created_at) OR (r.created_at = sqlc.arg(after_created_at) AND r.id > sqlc.arg(after_id)))
  AND (s.runner_id IS NULL OR s.runner_id = sqlc.arg(runner_id))
  AND NOT EXISTS (
      SELECT 1 FROM runs o
      WHERE o.session_id = r.session_id
        AND o.state IN ('offered', 'claimed', 'preparing', 'running', 'waiting'))
  AND NOT EXISTS (
      SELECT 1 FROM runs e
      WHERE e.session_id = r.session_id AND e.state = 'queued'
        AND (e.created_at < r.created_at OR (e.created_at = r.created_at AND e.id < r.id)))
ORDER BY r.created_at, r.id
LIMIT sqlc.arg(max);

-- name: OfferRun :exec
UPDATE runs SET state = 'offered', runner_id = ?, lease_expires_at = ?, updated_at = ?
WHERE id = ? AND state = 'queued';

-- name: RunsOfferedTo :many
SELECT * FROM runs WHERE runner_id = ? AND state = 'offered';

-- name: RequeueRun :exec
-- An offer the runner did not list back was never received: the run goes back
-- in the queue, for this runner or another.
UPDATE runs SET state = 'queued', runner_id = NULL, lease_expires_at = NULL, updated_at = ?
WHERE id = ? AND state = 'offered';

-- name: RenewRun :exec
-- Listing a run claims it (when offered) and renews its lease (always).
UPDATE runs SET state = ?, lease_expires_at = ?, resumes_at = ?, reason = ?, updated_at = ?
WHERE id = ? AND runner_id = ?;

-- name: RequeueWithdrawnOffers :execrows
UPDATE runs SET state = 'queued', runner_id = NULL, lease_expires_at = NULL, updated_at = sqlc.arg(now)
WHERE state = 'offered' AND lease_expires_at <= sqlc.arg(now);

-- name: LoseLapsedRuns :execrows
UPDATE runs SET state = 'lost', reason = 'the runner stopped syncing and its lease lapsed', updated_at = sqlc.arg(now)
WHERE state IN ('claimed', 'preparing', 'running', 'waiting') AND lease_expires_at <= sqlc.arg(now);

-- name: AppendEvent :execrows
INSERT INTO events (run_id, seq, body, received_at) VALUES (?, ?, ?, ?)
ON CONFLICT (run_id, seq) DO NOTHING;

-- name: EventsAfter :many
SELECT seq, body FROM events WHERE run_id = ? AND seq > ? ORDER BY seq LIMIT ?;

-- name: PutResult :execrows
INSERT INTO results (run_id, state, body, received_at) VALUES (?, ?, ?, ?)
ON CONFLICT (run_id) DO NOTHING;

-- name: GetResult :one
SELECT * FROM results WHERE run_id = ?;

-- Admin tokens and the service API (DEV-7). Kept in one block so the runner's
-- events and result queries can land beside it without a merge fight.

-- name: CreateAdminToken :exec
INSERT INTO admin_tokens (hash, name, created_at) VALUES (?, ?, ?);

-- name: GetAdminToken :one
SELECT * FROM admin_tokens WHERE hash = ?;

-- name: ListAdminTokens :many
SELECT name, created_at FROM admin_tokens ORDER BY name;

-- name: RevokeAdminToken :execrows
DELETE FROM admin_tokens WHERE name = ?;

-- Events and results (DEV-6): the protocol's two reporting calls.

-- name: EventSeqsFrom :many
SELECT seq FROM events WHERE run_id = sqlc.arg(run_id) AND seq > sqlc.arg(after) ORDER BY seq LIMIT sqlc.arg(max);

-- name: SetEventsThrough :exec
UPDATE runs SET events_through = ? WHERE id = ?;

-- The result settles the run: its state, the reason a failure gave, and no
-- lease any more, so a finished run cannot lapse into lost.
-- name: FinishRun :exec
UPDATE runs SET state = ?, reason = ?, lease_expires_at = NULL, resumes_at = NULL, updated_at = ?
WHERE id = ?;
