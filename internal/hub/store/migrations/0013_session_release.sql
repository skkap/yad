-- Releasing a session (decision 0080). The service API asks for one; the
-- release_session control goes out in every answer to the bound runner's
-- syncs until it reports the session in released_sessions. While it is asked
-- for, the session's queued runs are held back, as a close holds them. A
-- release the runner reports leaves the session unbound, with its archive
-- here, and its next run goes out opening it, with the archive, to any runner
-- advertising release for its harness; a refusal leaves it bound as it was.
ALTER TABLE sessions ADD COLUMN release_requested_at INTEGER;
ALTER TABLE sessions ADD COLUMN release_discard_local INTEGER NOT NULL DEFAULT 0;
-- The last release its runner refused, as protocol/v1 ReleaseError JSON,
-- kept until the next is asked for.
ALTER TABLE sessions ADD COLUMN release_error TEXT;
-- When the session was last released, and from which runner: history once a
-- claim binds it again.
ALTER TABLE sessions ADD COLUMN released_at INTEGER;
ALTER TABLE sessions ADD COLUMN released_from TEXT REFERENCES runners (id);

-- A session's archive: opaque bytes this hub stores and hands back, the
-- conversation of a session whose runner released it. Uploaded by the runner
-- the session is bound to while its release is asked for, and handed only to
-- the runner a run of the session is offered to or bound to. Gone once a run
-- of the session is running on the runner that opened it, once the session
-- closes, or once it is older than the hub's archive TTL; the hub database is
-- opened with secure_delete, so its bytes are zeroed when it goes.
CREATE TABLE session_archives (
    session_id  TEXT PRIMARY KEY REFERENCES sessions (id),
    sha256      TEXT NOT NULL,
    bytes       INTEGER NOT NULL,
    data        BLOB NOT NULL,
    runner_id   TEXT NOT NULL REFERENCES runners (id),
    uploaded_at INTEGER NOT NULL,
    -- Set when the runner reported the release: from then the archive is
    -- the session's conversation, and its next run opens from it.
    released    INTEGER NOT NULL DEFAULT 0
);

-- A closed session takes no run, so nothing will open from its archive: a
-- trigger rather than a call in every path that closes one, for the reason
-- runs_forget_grants gives.
CREATE TRIGGER sessions_forget_archive AFTER UPDATE OF closed_at ON sessions
WHEN NEW.closed_at IS NOT NULL
BEGIN
    DELETE FROM session_archives WHERE session_id = NEW.id;
END;
