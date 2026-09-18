-- yad hub's durable state, separate from any runner's state.db: a hub and a
-- runner on one machine are two parties that trust each other no more than
-- two machines would. Times are unix milliseconds, as in the runner's store.
--
-- Secrets are stored only as SHA-256 hashes. Both kinds are 256 random bits,
-- so a fast hash is enough — there is no password to brute-force — and a
-- leaked database gives nobody a token or a credential.

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
    -- JSON. Grants are the hub's to hold until the run ends.
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
