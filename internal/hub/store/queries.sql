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

-- name: ListRunners :many
-- Every runner this hub knows. The ones still talking to it come first, so an
-- operator reading the list sees the live fleet before the retired machines;
-- a runner that registered and never synced sorts last, by id.
SELECT * FROM runners ORDER BY last_sync_at IS NULL, last_sync_at DESC, id;

-- name: RecordSync :exec
UPDATE runners SET last_sync_at = ?, health = ?, wants_capabilities = ? WHERE id = ?;

-- name: SetCapabilities :exec
UPDATE runners SET capabilities = ?, fingerprint = ?, wants_capabilities = 0 WHERE id = ?;

-- name: CreateSession :exec
INSERT INTO sessions (id, harness, created_at) VALUES (?, ?, ?)
ON CONFLICT (id) DO NOTHING;

-- name: GetSession :one
SELECT * FROM sessions WHERE id = ?;

-- name: SessionCreatorSpec :one
-- The spec of the run whose submission created the session: the one its
-- submitter sent as new. Found by that flag rather than by order, since a
-- continuation submitted in the same millisecond can sort ahead of it.
SELECT spec FROM runs WHERE session_id = ? AND json_extract(spec, '$.session.new') = 1
ORDER BY created_at, id LIMIT 1;

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
-- session has at most one live run. Nor in a session whose close is asked
-- for: the runner acts on close_session before the offers beside it and would
-- refuse the run, which instead ends with the close once reported (DEV-120).
-- Filtering here rather than in Go is what keeps runs it must skip from
-- filling the page ahead of runs it could take.
SELECT r.* FROM runs r JOIN sessions s ON s.id = r.session_id
WHERE r.state = 'queued'
  AND s.close_requested_at IS NULL AND s.closed_at IS NULL
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

-- name: SoonCandidates :many
-- Queued runs this runner would be offered once a run it holds ends: the
-- rules of OfferCandidates, except that a session bound to it may have a run
-- out, since that run is this runner's and ending it is what lets the next
-- one go. Not a waiting one: that ends at an account's reset, hours off, not
-- when anything the runner is executing does. An unbound session with a run
-- out is left out: that run may be on its way to another runner, whose claim
-- would bind the session there.
SELECT r.* FROM runs r JOIN sessions s ON s.id = r.session_id
WHERE r.state = 'queued'
  AND s.close_requested_at IS NULL AND s.closed_at IS NULL
  AND r.harness IN (SELECT value FROM json_each(sqlc.arg(harnesses_json)))
  AND (r.created_at > sqlc.arg(after_created_at) OR (r.created_at = sqlc.arg(after_created_at) AND r.id > sqlc.arg(after_id)))
  AND ((s.runner_id = sqlc.arg(runner_id) AND NOT EXISTS (
      SELECT 1 FROM runs w WHERE w.session_id = r.session_id AND w.state = 'waiting'))
    OR (s.runner_id IS NULL AND NOT EXISTS (
      SELECT 1 FROM runs o
      WHERE o.session_id = r.session_id
        AND o.state IN ('offered', 'claimed', 'preparing', 'running', 'waiting'))))
  AND NOT EXISTS (
      SELECT 1 FROM runs e
      WHERE e.session_id = r.session_id AND e.state = 'queued'
        AND (e.created_at < r.created_at OR (e.created_at = r.created_at AND e.id < r.id)))
ORDER BY r.created_at, r.id
LIMIT sqlc.arg(max);

-- name: OfferRun :exec
UPDATE runs SET state = 'offered', runner_id = ?, lease_expires_at = ?, updated_at = ?
WHERE id = ? AND state = 'queued';

-- name: NoteSessionOffer :exec
-- Until a claim binds it, a session remembers the runner its run was last
-- offered to: the one runner whose report of closing it is believed.
UPDATE sessions SET offered_to = sqlc.arg(runner_id) WHERE id = sqlc.arg(id) AND runner_id IS NULL;

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

-- A watcher reads only as far as the stream is contiguous: an event stored
-- past a gap waits until the gap is filled, or a cursor would move past the
-- missing seq and never see it.
-- name: EventsContiguous :many
SELECT e.seq, e.body FROM events e JOIN runs r ON r.id = e.run_id
WHERE e.run_id = sqlc.arg(run_id) AND e.seq > sqlc.arg(after) AND e.seq <= r.events_through
ORDER BY e.seq LIMIT sqlc.arg(max);

-- Cancel, interrupt and steer (DEV-8).

-- name: AddControl :exec
INSERT INTO run_controls (run_id, kind, text, created_at) VALUES (?, ?, ?, ?);

-- name: FirstControl :one
SELECT * FROM run_controls WHERE run_id = ? AND kind = ? ORDER BY id LIMIT 1;

-- name: ControlsFor :many
SELECT * FROM run_controls WHERE run_id = ? ORDER BY id;

-- name: DeleteSteersThrough :exec
DELETE FROM run_controls WHERE run_id = ? AND kind = 'steer' AND id <= ?;

-- A run no runner has started ends on the hub alone: cancelled when someone
-- asked, failed when what it needed has gone. An offered run keeps its
-- runner: that runner lists it once more, hears cancel, and withdraws it.
-- name: EndUnstartedRun :execrows
UPDATE runs SET state = sqlc.arg(state), reason = sqlc.arg(reason), lease_expires_at = NULL, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state IN ('queued', 'offered');

-- name: RequestDrain :exec
UPDATE runners SET drain_requested_at = COALESCE(drain_requested_at, sqlc.arg(now)) WHERE id = sqlc.arg(id);

-- name: ClearDrain :exec
UPDATE runners SET drain_requested_at = NULL WHERE id = ?;

-- name: RequestSessionClose :exec
UPDATE sessions SET close_requested_at = COALESCE(close_requested_at, sqlc.arg(now))
WHERE id = sqlc.arg(id) AND closed_at IS NULL;

-- A runner's report closes a session bound to it, or an unbound one whose
-- run was last offered to it: its claim may have been withdrawn before any
-- sync listed it, and the close its owner asked for meanwhile is still owed
-- (DEV-103). Such a session is bound to the reporter as it closes, since
-- whatever it left is on that runner's disk. A repeat changes nothing, and a
-- report from any other runner is not applied.
-- name: RecordSessionClosed :execrows
UPDATE sessions SET closed_at = sqlc.arg(closed_at), close_reason = sqlc.arg(reason), close_requested_at = NULL,
  runner_id = sqlc.arg(runner_id)
WHERE id = sqlc.arg(id) AND closed_at IS NULL
  AND (runner_id = sqlc.arg(runner_id) OR (runner_id IS NULL AND offered_to = sqlc.arg(runner_id)));

-- The hub closing a session by its own act (decision 0011), without waiting
-- for a runner to report it: one nobody ever claimed, or one whose runner
-- deregistered or went silent. A silent runner may still come back, and its
-- caller marks the close owed to it (OweSessionClose, decision 0046). A
-- repeat changes nothing.
-- name: CloseSessionHere :execrows
UPDATE sessions SET closed_at = sqlc.arg(now), close_reason = sqlc.arg(reason), close_requested_at = NULL
WHERE id = sqlc.arg(id) AND closed_at IS NULL;

-- The sessions close_session goes to this runner for: a close asked for and
-- not yet reported, and one the hub closed while the runner was silent, whose
-- workdir the runner still holds (decision 0046).
-- name: SessionsToClose :many
SELECT id FROM sessions
WHERE runner_id = sqlc.arg(runner_id) AND (close_requested_at IS NOT NULL AND closed_at IS NULL OR close_owed = 1)
ORDER BY id;

-- The hub closed it by its own act and the runner has not heard yet: the
-- close_session goes out until the runner reports the session closed.
-- name: OweSessionClose :exec
UPDATE sessions SET close_owed = 1 WHERE id = sqlc.arg(id) AND runner_id IS NOT NULL;

-- The runner holding the session reported it closed, which answers every
-- close_session it was owed. A report from any other runner answers nothing.
-- name: SettleOwedClose :exec
UPDATE sessions SET close_owed = 0 WHERE id = sqlc.arg(id) AND runner_id = sqlc.arg(runner_id) AND close_owed = 1;

-- name: UnstartedRunsInSession :many
SELECT id FROM runs WHERE session_id = ? AND state IN ('queued', 'offered') ORDER BY created_at, id;

-- Deregistration. A runner that deregisters keeps its row: sessions and runs
-- reference it, and re-registering under a token issued for that id is how it
-- comes back. What goes is its credential, replaced by the hash of a secret
-- nobody was given, so nothing can authenticate as it again.
-- name: RetireRunnerCredential :exec
UPDATE runners SET credential_hash = sqlc.arg(credential_hash), drain_requested_at = NULL
WHERE id = sqlc.arg(id);

-- Runs a departed runner held are lost: it is not coming back to report
-- them, and lost is what a hub tells a submitter then (decision 0023).
-- name: LoseRunnerRuns :execrows
UPDATE runs SET state = 'lost', reason = sqlc.arg(reason), lease_expires_at = NULL, resumes_at = NULL,
  updated_at = sqlc.arg(now)
WHERE runner_id = sqlc.arg(runner_id) AND state IN ('claimed', 'preparing', 'running', 'waiting');

-- An offer it never claimed was never held, so it goes back in the queue
-- rather than being lost.
-- name: RequeueRunnerOffers :execrows
UPDATE runs SET state = 'queued', runner_id = NULL, lease_expires_at = NULL, updated_at = sqlc.arg(now)
WHERE runner_id = sqlc.arg(runner_id) AND state = 'offered';

-- A departed runner's sessions that still need the hub: the open ones. A
-- closed one holds no waiting run, because every close ends those.
-- name: SessionsToSettle :many
SELECT id FROM sessions WHERE runner_id = sqlc.arg(runner_id) AND closed_at IS NULL ORDER BY id;

-- Runners silent since before the cutoff that still have an open session
-- bound to them (decision 0046). Silence counts from the last sync the hub
-- answered; a runner that never synced has no session bound to it, because
-- only a sync's listing binds one.
-- name: SilentRunners :many
SELECT DISTINCT r.id FROM runners r JOIN sessions s ON s.runner_id = r.id
WHERE s.closed_at IS NULL AND r.last_sync_at IS NOT NULL AND r.last_sync_at <= sqlc.arg(cutoff)
ORDER BY r.id;

-- Hub logins (decision 0055).

-- name: CreateLogin :exec
INSERT INTO logins (id, runner_id, harness, account, method, token, add_account, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetLogin :one
SELECT * FROM logins WHERE id = ?;

-- Every login of this runner not yet over, oldest first.
-- name: OpenLogins :many
SELECT * FROM logins WHERE runner_id = ? AND state IN ('requested', 'starting', 'waiting', 'checking')
ORDER BY created_at, id;

-- The open logins of one account on one runner: what a new login for it
-- replaces.
-- name: OpenLoginsFor :many
SELECT * FROM logins WHERE runner_id = sqlc.arg(runner_id) AND harness = sqlc.arg(harness) AND account = sqlc.arg(account)
  AND state IN ('requested', 'starting', 'waiting', 'checking')
ORDER BY created_at, id;

-- A sync's answer carries the login's start: from now the runner may have it.
-- name: MarkLoginSent :exec
UPDATE logins SET sent_at = COALESCE(sent_at, sqlc.arg(now)) WHERE id = sqlc.arg(id);

-- A runner's report of a login this hub started on it. An end the runner
-- reported stands; an end the hub made by its own act gives way to the
-- runner's own report of an end, which knows what happened on the machine.
-- A report about another runner's login changes nothing. updated_at moves
-- only with the state, so it says when the login last did.
-- name: RecordLoginReport :execrows
UPDATE logins SET state = sqlc.arg(state), url = sqlc.arg(url), user_code = sqlc.arg(user_code), error = sqlc.arg(error),
  hub_ended = 0,
  updated_at = CASE WHEN state = sqlc.arg(state) THEN updated_at ELSE sqlc.arg(now) END
WHERE id = sqlc.arg(id) AND runner_id = sqlc.arg(runner_id)
  AND (state IN ('requested', 'starting', 'waiting', 'checking')
    OR hub_ended = 1 AND CAST(sqlc.arg(terminal) AS INTEGER) = 1);

-- name: SetLoginCode :exec
UPDATE logins SET code = sqlc.arg(code) WHERE id = sqlc.arg(id) AND state = 'waiting';

-- name: RequestLoginCancel :exec
UPDATE logins SET cancel_requested_at = COALESCE(cancel_requested_at, sqlc.arg(now))
WHERE id = sqlc.arg(id) AND state IN ('requested', 'starting', 'waiting', 'checking');

-- The hub ending a login by its own act: only ever one no answer has
-- carried to its runner, or one its runner can no longer report.
-- name: EndLogin :execrows
UPDATE logins SET state = sqlc.arg(state), error = sqlc.arg(error), url = '', hub_ended = 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state IN ('requested', 'starting', 'waiting', 'checking');

-- Logins no answer carried to their runner by the cutoff: it is not syncing,
-- and a token must not wait in this database for it to come back. One an
-- answer did carry is the runner's, and falls to ExpireStaleLogins.
-- name: ExpireUndeliveredLogins :execrows
UPDATE logins SET state = 'expired', error = sqlc.arg(error), hub_ended = 1, updated_at = sqlc.arg(now)
WHERE state = 'requested' AND sent_at IS NULL AND created_at <= sqlc.arg(cutoff);

-- Logins sent to a runner and not finished by the cutoff, which is well past
-- every deadline a runner holds one to: it stopped reporting them.
-- name: ExpireStaleLogins :execrows
UPDATE logins SET state = 'failed', error = sqlc.arg(error), url = '', hub_ended = 1, updated_at = sqlc.arg(now)
WHERE (state IN ('starting', 'waiting', 'checking') OR state = 'requested' AND sent_at IS NOT NULL)
  AND created_at <= sqlc.arg(cutoff);

-- A runner that deregisters finishes none of its logins.
-- name: EndRunnerLogins :execrows
UPDATE logins SET state = 'failed', error = sqlc.arg(error), url = '', hub_ended = 1, updated_at = sqlc.arg(now)
WHERE runner_id = sqlc.arg(runner_id) AND state IN ('requested', 'starting', 'waiting', 'checking');

-- Removing a runner's accounts (decision 0057).

-- A repeat keeps the first request's time: it is the same removal.
-- name: RequestAccountRemoval :exec
INSERT INTO account_removals (runner_id, harness, account, requested_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (runner_id, harness, account) DO NOTHING;

-- name: GetAccountRemoval :one
SELECT * FROM account_removals WHERE runner_id = ? AND harness = ? AND account = ?;

-- name: AccountRemovals :many
SELECT * FROM account_removals WHERE runner_id = ? ORDER BY requested_at, harness, account;

-- name: EndAccountRemoval :exec
DELETE FROM account_removals WHERE runner_id = ? AND harness = ? AND account = ?;
