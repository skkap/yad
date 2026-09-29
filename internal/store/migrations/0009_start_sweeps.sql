-- Work a migration owes that SQL cannot do, left for the daemon's next start
-- by name. The daemon does it in Go and deletes the row in the same
-- transaction, so it is done once, whole or not at all, and a later start
-- pays one lookup for it. Only the daemon migrates state.db and only it
-- writes it (decision 0043), so it is the daemon that finds the row.
CREATE TABLE start_sweeps (
    name TEXT PRIMARY KEY
);

-- Decision 0068: a version before it kept the credential an https source
-- URL carried as its userinfo in runs.spec and sessions.sources, and in the
-- text that quoted the URL: events, a run's reason, the outbox. Finding it
-- means reading each git source's URL out of JSON, which is
-- runner.scrubSourceCredentials's, with internal/workdir's rules for what a
-- credential is.
INSERT INTO start_sweeps (name) VALUES ('source_credentials');
