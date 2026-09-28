-- The run whose claim bound the session (DEV-143). A claim the hub was asked
-- to cancel and the runner withdrew takes the session it opened with it on
-- the runner (decision 0061), so the hub unbinds the session when that claim
-- was the one that bound it: the next run goes out opening the session,
-- rather than continuing one no runner holds. Recorded only from here on; a
-- session bound before this has none, and is never unbound.
ALTER TABLE sessions ADD COLUMN bound_by_run TEXT REFERENCES runs (id);
