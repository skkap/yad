-- The runner's durable state. Times are unix milliseconds: one representation,
-- no timezone parsing, and sqlc maps them to int64 without custom types.

CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,
    connection   TEXT NOT NULL,
    harness      TEXT NOT NULL,
    -- Written the moment the harness reveals it, not at the end of the run:
    -- a crash must not lose the resume pointer.
    native_id    TEXT,
    account      TEXT,
    workdir      TEXT NOT NULL,
    state        TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'closed', 'expired')),
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL
);

CREATE TABLE runs (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions (id),
    connection  TEXT NOT NULL,
    harness     TEXT NOT NULL,
    model       TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL CHECK (state IN (
        'claimed', 'preparing', 'running', 'waiting',
        'succeeded', 'failed', 'cancelled', 'timed_out', 'lost')),
    -- The run as the hub sent it, grants removed: grants never touch disk here.
    spec        TEXT NOT NULL,
    account     TEXT,
    resumes_at  INTEGER,
    reason      TEXT,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

-- At most one live run per session. Two harness processes resuming one
-- transcript corrupt it silently (yashiki's "resume hazard"), so the database
-- refuses it rather than trusting every code path to check first.
CREATE UNIQUE INDEX runs_one_live_per_session ON runs (session_id)
    WHERE state IN ('claimed', 'preparing', 'running', 'waiting');

-- The event spool: every event a run produced, until the hub acknowledges it.
-- (run_id, seq) is the idempotency key the protocol promises.
CREATE TABLE events (
    run_id TEXT NOT NULL REFERENCES runs (id),
    seq    INTEGER NOT NULL,
    body   TEXT NOT NULL,
    acked  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, seq)
);

-- Terminal results not yet acknowledged. Written before the first attempt and
-- deleted only on a 2xx, so a result survives any crash or network loss.
CREATE TABLE outbox (
    run_id          TEXT PRIMARY KEY REFERENCES runs (id),
    connection      TEXT NOT NULL,
    body            TEXT NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    last_error      TEXT
);

CREATE TABLE accounts (
    harness       TEXT NOT NULL,
    label         TEXT NOT NULL,
    limited_until INTEGER,
    PRIMARY KEY (harness, label)
);

-- WT_SLOT allocation: unique among one repository's live worktrees, recycled
-- when a workdir is reclaimed.
CREATE TABLE slots (
    repo       TEXT NOT NULL,
    slot       INTEGER NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions (id),
    PRIMARY KEY (repo, slot)
);
