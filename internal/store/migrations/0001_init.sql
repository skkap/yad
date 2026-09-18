-- The runner's durable state. Times are unix milliseconds: one representation,
-- no timezone parsing, and sqlc maps them to int64 without custom types.
--
-- Session and run ids are chosen by hubs, and hubs do not coordinate: two of
-- them may legally pick the same id. So every hub-issued id is keyed together
-- with the connection it came from, and nothing in one connection can name a
-- row belonging to another.

CREATE TABLE sessions (
    connection   TEXT NOT NULL,
    id           TEXT NOT NULL,
    harness      TEXT NOT NULL,
    -- Written the moment the harness reveals it, not at the end of the run:
    -- a crash must not lose the resume pointer.
    native_id    TEXT,
    account      TEXT,
    workdir      TEXT NOT NULL,
    state        TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'closed', 'expired')),
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL,
    PRIMARY KEY (connection, id)
);

CREATE TABLE runs (
    connection  TEXT NOT NULL,
    id          TEXT NOT NULL,
    session_id  TEXT NOT NULL,
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
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (connection, id),
    FOREIGN KEY (connection, session_id) REFERENCES sessions (connection, id)
);

-- At most one live run per session. Two harness processes resuming one
-- transcript corrupt it silently (yashiki's "resume hazard"), so the database
-- refuses it rather than trusting every code path to check first.
CREATE UNIQUE INDEX runs_one_live_per_session ON runs (connection, session_id)
    WHERE state IN ('claimed', 'preparing', 'running', 'waiting');

-- The event spool: every event a run produced, until the hub acknowledges it.
-- (run_id, seq) is the idempotency key the protocol promises, within one hub.
CREATE TABLE events (
    connection TEXT NOT NULL,
    run_id     TEXT NOT NULL,
    seq        INTEGER NOT NULL,
    body       TEXT NOT NULL,
    acked      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (connection, run_id, seq),
    FOREIGN KEY (connection, run_id) REFERENCES runs (connection, id)
);

-- Terminal results not yet acknowledged. Written before the first attempt and
-- deleted only on a 2xx, so a result survives any crash or network loss.
CREATE TABLE outbox (
    connection      TEXT NOT NULL,
    run_id          TEXT NOT NULL,
    body            TEXT NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    last_error      TEXT,
    PRIMARY KEY (connection, run_id),
    FOREIGN KEY (connection, run_id) REFERENCES runs (connection, id)
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
    connection TEXT NOT NULL,
    session_id TEXT NOT NULL,
    PRIMARY KEY (repo, slot),
    FOREIGN KEY (connection, session_id) REFERENCES sessions (connection, id)
);
