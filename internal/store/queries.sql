-- Every hub-issued id (session, run) is addressed together with its
-- connection; see the note at the top of migrations/0001_init.sql.

-- ASCII only in this file, comments included. sqlc 1.31.1 rewrites each query
-- using byte offsets it computed over runes, so one multi-byte character
-- anywhere above a query truncates that query's text and every later one:
-- `SELECT *` arrives at its parser as `SELECharness` and generation fails
-- naming queries nobody touched. The repo's prose uses an em dash; here it
-- costs an afternoon. (Confirmed by adding one and removing it again, DEV-27.)

-- name: CreateSession :exec
INSERT INTO sessions (connection, id, harness, account, workdir, created_at, last_used_at, fork_from)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

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
INSERT INTO runs (connection, id, session_id, harness, model, state, spec, had_grants, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'claimed', ?, ?, ?, ?);

-- name: GetRun :one
SELECT * FROM runs WHERE connection = ? AND id = ?;

-- name: SetRunState :exec
UPDATE runs SET state = ?, resumes_at = ?, reason = ?, updated_at = ? WHERE connection = ? AND id = ?;

-- The hub answered a sync listing this claim without cancelling it, which is
-- when it bound the run's session to this runner.
-- name: AcknowledgeClaim :exec
UPDATE runs SET acknowledged = 1 WHERE connection = ? AND id = ?;

-- The moment the run first reached preparing, kept so a run that waited
-- between two turns still reports the whole of its duration. Written once:
-- a resumed run is the same run, not a new one.
-- name: SetRunStarted :exec
UPDATE runs SET started_at = ?, updated_at = ? WHERE connection = ? AND id = ? AND started_at IS NULL;

-- Park the run on a usage limit. The state, the moment it comes back and when
-- the wait began, in one statement: a park missing any of them is a run no
-- sync can finish. What the run has cost is not here, because it is already
-- on the row: SetRunSpent wrote it when the turn before the park ended, and
-- SetRunAccount wrote the moves.
-- name: SetRunWaiting :exec
UPDATE runs SET state = 'waiting', resumes_at = ?, waiting_since = ?, reason = ?, updated_at = ?
WHERE connection = ? AND id = ?;

-- What the run's turns have cost, written as each turn ends. Once per turn
-- and never per event: a turn's usage arrives with its end, so nothing
-- written sooner would hold more. It is what a restart reports for a run it
-- finds lost (decision 0030), and what a later process resumes a parked run
-- from.
-- name: SetRunSpent :exec
UPDATE runs SET spent = ?, updated_at = ? WHERE connection = ? AND id = ?;

-- End the current wait, folding it into the total. Called by whoever takes
-- the run out of waiting, before it runs again or times out, so the wait in
-- progress is counted exactly once whichever of the two happens.
-- name: EndRunWait :exec
UPDATE runs SET waited_ms = waited_ms + MAX(sqlc.arg(now) - COALESCE(waiting_since, sqlc.arg(now)), 0),
  waiting_since = NULL, resumes_at = NULL, updated_at = sqlc.arg(now)
WHERE connection = sqlc.arg(connection) AND id = sqlc.arg(id);

-- Every parked run, across connections, for the collector: it ends the ones
-- whose cap has run out on a connection no sync loop is serving, and it never
-- starts one. A loop resuming its own connection's parked runs reads them from
-- the listing it already makes. Ordered oldest first so the run that has waited
-- longest is dealt with first.
-- name: ListWaitingRuns :many
SELECT * FROM runs WHERE state = 'waiting' ORDER BY created_at, connection, id;

-- The account a turn is about to run on, and how many moves it took to get
-- there. Together because a move is counted when the next account is taken:
-- written apart, a restart between the two would find the run on a new
-- account with the move that put it there uncounted.
-- name: SetRunAccount :exec
UPDATE runs SET account = ?, account_switches = ?, updated_at = ? WHERE connection = ? AND id = ?;

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

-- A session a withdrawn claim opened goes with it, so the hub can offer the
-- run again. Not one with a close asked for: that close was answered
-- "closing" and must still happen and be reported (decision 0035), so
-- CloseRequestedEmptySession closes it instead. Deleting it would lose the
-- close and let the re-offer open the session afresh.
-- name: DeleteEmptySession :exec
DELETE FROM sessions WHERE sessions.connection = sqlc.arg(connection) AND sessions.id = sqlc.arg(id)
  AND sessions.state = 'open' AND sessions.close_requested_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.connection = sessions.connection AND r.session_id = sessions.id);

-- A close that waited on a claim which was then withdrawn: nothing is held in
-- the session any more, so it closes in the withdrawal's own transaction, with
-- the reason first asked for, and the next sync reports it. The state follows
-- the reason as the collector's close does.
-- name: CloseRequestedEmptySession :execrows
UPDATE sessions SET state = CASE WHEN close_reason IN ('expired', 'disk_pressure') THEN 'expired' ELSE 'closed' END,
  closed_at = sqlc.arg(now), close_requested_at = NULL
WHERE sessions.connection = sqlc.arg(connection) AND sessions.id = sqlc.arg(id) AND sessions.state = 'open'
  AND sessions.close_requested_at IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.connection = sessions.connection AND r.session_id = sessions.id
    AND r.state IN ('claimed', 'preparing', 'running', 'waiting'));

-- The last event a run spooled, acknowledged or not: a result's last_seq.
-- name: LastEventSeq :one
SELECT CAST(COALESCE(MAX(seq), 0) AS INTEGER) FROM events WHERE connection = ? AND run_id = ?;

-- The last event a run spooled, whole. The body carries the moment the event
-- was written, which is the latest time anything is known to have been true
-- of a run whose process is gone: runs.updated_at moves only when a column
-- does, so for a turn that streamed for three hours it still says when the
-- run reached running. No rows for a run that never spoke.
-- name: LastEvent :one
SELECT seq, body FROM events WHERE connection = ? AND run_id = ? ORDER BY seq DESC LIMIT 1;

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
  s.close_reason, s.close_requested_at, s.closed_at, s.reclaimed_at, s.reported_at, s.fork_from,
  CAST(COALESCE((SELECT r.id FROM runs r
    WHERE r.connection = s.connection AND r.session_id = s.id
      AND r.state IN ('claimed', 'preparing', 'running', 'waiting')), '') AS TEXT) AS live_run,
  CAST((SELECT count(*) FROM runs r WHERE r.connection = s.connection AND r.session_id = s.id) AS INTEGER) AS runs
FROM sessions s
ORDER BY s.last_used_at DESC, s.connection, s.id;

-- The run that opened a session here: the first one recorded in it.
-- name: SessionOpenerSpec :one
SELECT spec FROM runs WHERE connection = ? AND session_id = ? ORDER BY created_at, id LIMIT 1;

-- name: SetSessionSources :exec
UPDATE sessions SET sources = ? WHERE connection = ? AND id = ?;

-- A sweep a migration left for the daemon's start (0009_start_sweeps.sql).
-- name: StartSweepPending :one
SELECT EXISTS (SELECT 1 FROM start_sweeps WHERE name = ?);

-- name: StartSweepDone :exec
DELETE FROM start_sweeps WHERE name = ?;

-- The rows whose JSON may name a git URL with userinfo, for the source
-- credential sweep to read (decision 0068). Every such URL has an '@'.
-- name: RunSpecsWithAt :many
SELECT connection, id, spec FROM runs WHERE instr(spec, '@') > 0;

-- name: SessionSourcesWithAt :many
SELECT connection, id, sources FROM sessions WHERE instr(sources, '@') > 0;

-- A run whose source carried a credential cannot be rebuilt from its row
-- once the credential is out of it, as Loop.record has marked such a run
-- since decision 0068.
-- name: MarkRunHadGrants :exec
UPDATE runs SET had_grants = 1 WHERE connection = ? AND id = ?;

-- Every copy of a source credential an earlier version wrote, replaced where
-- it stands: the rest of the text keeps its bytes, which a JSON parse and
-- re-encode would not promise.
-- name: ScrubRuns :execrows
UPDATE runs SET spec = replace(spec, sqlc.arg(old), sqlc.arg(new)), reason = replace(reason, sqlc.arg(old), sqlc.arg(new))
WHERE instr(spec, sqlc.arg(old)) > 0 OR instr(reason, sqlc.arg(old)) > 0;

-- name: ScrubSessions :execrows
UPDATE sessions SET sources = replace(sources, sqlc.arg(old), sqlc.arg(new))
WHERE instr(sources, sqlc.arg(old)) > 0;

-- name: ScrubEvents :execrows
UPDATE events SET body = replace(body, sqlc.arg(old), sqlc.arg(new))
WHERE instr(body, sqlc.arg(old)) > 0;

-- name: ScrubOutbox :execrows
UPDATE outbox SET body = replace(body, sqlc.arg(old), sqlc.arg(new)), last_error = replace(last_error, sqlc.arg(old), sqlc.arg(new))
WHERE instr(body, sqlc.arg(old)) > 0 OR instr(last_error, sqlc.arg(old)) > 0;

-- A bare cache moved to the name it has now keeps its sessions' WT_SLOTs.
-- OR IGNORE: a slot the new name already holds stays that session's, and the
-- one under the old name is freed with its session.
-- name: RenameSlotsRepo :exec
UPDATE OR IGNORE slots SET repo = sqlc.arg(new_repo) WHERE repo = sqlc.arg(old_repo);
