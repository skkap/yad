-- yad hub's durable state, separate from any runner's state.db: a hub and a
-- runner on one machine are two parties that trust each other no more than
-- two machines would. Times are unix milliseconds, as in the runner's store.
--
-- All three secret kinds — registration token, runner credential, admin token
-- — are stored only as SHA-256 hashes. Each is 256 random bits, so a fast hash
-- is enough: there is no password to brute-force, and a leaked database gives
-- none of the three back. It does give up the grants of every run that has
-- not ended yet, which are plaintext inside runs.spec below; see the note
-- there.

CREATE TABLE registration_tokens (
    hash       TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    -- Set when the token was issued to re-register one runner that already
    -- exists: only such a token may replace a runner's credential, so a
    -- token for "a new machine" cannot take over a runner somebody else holds.
    for_runner TEXT,
    -- Set once, by the exchange that burns it; a second exchange finds it set.
    used_at    INTEGER,
    runner_id  TEXT
);

CREATE TABLE runners (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    credential_hash TEXT NOT NULL UNIQUE,
    -- The last full capability document, as JSON, and the fingerprint the
    -- runner sent with it. A sync whose fingerprint differs sets
    -- wants_capabilities, and the next response asks for the document.
    capabilities       TEXT NOT NULL,
    fingerprint        TEXT NOT NULL,
    wants_capabilities INTEGER NOT NULL DEFAULT 0,
    -- The last sync's health, as JSON: routing reads free capacity from it.
    health          TEXT,
    registered_at   INTEGER NOT NULL,
    last_sync_at    INTEGER
);

-- A session is resumable only on the runner that holds it, so the first claim
-- binds it and every later run in it is offered to that runner alone.
CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    harness    TEXT NOT NULL,
    runner_id  TEXT REFERENCES runners (id),
    created_at INTEGER NOT NULL
);

-- Hub-side run states extend the protocol's with two that exist only here:
-- queued (waiting for a runner) and offered (sent in a sync response, not yet
-- listed back). Everything from claimed on is the protocol's run state.
CREATE TABLE runs (
    id         TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions (id),
    harness    TEXT NOT NULL,
    model      TEXT NOT NULL,
    -- The run exactly as it will be offered — brief, sources and grants — as
    -- JSON. A grant's value is held only while the run can still use it
    -- (decision 0041): runs_forget_grants below blanks every value the moment
    -- the run reaches a terminal state, and keeps each grant's name and
    -- delivery, so the spec still says what the run was given. A waiting run
    -- is not terminal and keeps its values, because its resume needs them.
    spec       TEXT NOT NULL,
    state      TEXT NOT NULL CHECK (state IN (
        'queued', 'offered',
        'claimed', 'preparing', 'running', 'waiting',
        'succeeded', 'failed', 'cancelled', 'timed_out', 'lost')),
    runner_id  TEXT REFERENCES runners (id),
    -- For an offered run, when the offer is withdrawn; for a held one, when
    -- the lease lapses and the run is lost.
    lease_expires_at INTEGER,
    resumes_at INTEGER,
    reason     TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- A trigger rather than a query every terminal path must remember to call:
-- a run ends by a result, a cancel, a lapsed lease, a closed session or a
-- deregistered runner, and the next way it ends will be written by someone
-- who never read this. Anything that moves state onto a terminal value
-- passes through here. The grants are rebuilt in order with only "value"
-- emptied, so a reader still unmarshals a whole v1.Run. Each json() puts
-- back the JSON-ness a value loses crossing a subquery, without which the
-- list comes back as strings that hold objects. The order is an
-- ordered subquery rather than ORDER BY inside json_group_array because sqlc
-- 1.31.1 cannot parse the latter, and SQLite never flattens an ordered
-- subquery into an aggregate, so the rows reach it in that order.
CREATE TRIGGER runs_forget_grants AFTER UPDATE OF state ON runs
WHEN NEW.state IN ('succeeded', 'failed', 'cancelled', 'timed_out', 'lost')
  AND json_array_length(NEW.spec, '$.grants') > 0
BEGIN
    UPDATE runs SET spec = json_set(spec, '$.grants', json((
        SELECT json_group_array(json(blank)) FROM (
            SELECT json_set(g.value, '$.value', '') AS blank
            FROM json_each(NEW.spec, '$.grants') AS g ORDER BY g.key))))
    WHERE id = NEW.id;
END;

CREATE INDEX runs_by_state ON runs (state, created_at);
CREATE INDEX runs_by_runner ON runs (runner_id, state);

-- Every event a runner uploaded. (run_id, seq) is the protocol's idempotency
-- key: a resent batch lands on rows that already exist.
CREATE TABLE events (
    run_id      TEXT NOT NULL REFERENCES runs (id),
    seq         INTEGER NOT NULL,
    body        TEXT NOT NULL,
    received_at INTEGER NOT NULL,
    PRIMARY KEY (run_id, seq)
);

-- A run's terminal report. One per run: the first is applied, and a different
-- one later is the protocol's 409.
CREATE TABLE results (
    run_id      TEXT PRIMARY KEY REFERENCES runs (id),
    state       TEXT NOT NULL,
    body        TEXT NOT NULL,
    received_at INTEGER NOT NULL
);
