-- Releasing a session (decision 0080): its hub's release_session asks this
-- runner to hand the session back, so its next run can go to another runner.
-- One row per session asked about, kept until the hub has heard how it ended.
--
-- The runner builds the session archive once no run is held in the session --
-- the conversation and a manifest, in a 0600 file under <data>/releases --
-- and uploads it with the result outbox's patience: retried with backoff
-- until the hub answers, and again after a restart. The archive is built
-- before the first upload and kept until the hub has taken it, so a crash
-- between the two uploads the same bytes. Once the hub takes it, the session
-- closes here as released and its workdir and conversation are reclaimed; a
-- refusal leaves the session as it was. Either way the outcome is reported in
-- released_sessions until a sync carrying it is answered.
CREATE TABLE releases (
    connection    TEXT NOT NULL,
    session_id    TEXT NOT NULL,
    -- The hub's discard_local: release even what the remote does not have.
    discard_local INTEGER NOT NULL DEFAULT 0,
    requested_at  INTEGER NOT NULL,
    -- The archive once built: where it is, its sha256 and its size.
    archive       TEXT,
    sha256        TEXT,
    bytes         INTEGER,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,
    -- Settled: released (error NULL) or refused, error being the
    -- protocol/v1 ReleaseError as JSON.
    settled_at    INTEGER,
    error         TEXT,
    -- A sync carrying the outcome was answered by the session's hub.
    reported_at   INTEGER,
    PRIMARY KEY (connection, session_id)
);

-- A session opened from another runner's archive (session.archive) holds the
-- archive it is to be opened from, as protocol/v1 SessionArchive JSON, until
-- its conversation is in place: a run that could not import it fails, and the
-- session's next run tries again rather than starting the conversation empty.
ALTER TABLE sessions ADD COLUMN archive TEXT;
-- The branch each git source was on at the runner that released the session,
-- by URL, as JSON: a source naming no branch is checked out on it here.
ALTER TABLE sessions ADD COLUMN carried TEXT;
