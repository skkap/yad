-- Closing sessions (decision 0035). The service API asks for a close; the
-- close_session control goes out in every answer to the bound runner's syncs
-- until the runner reports the session in closed_sessions, and the report is
-- what closes it here. A runner also reports sessions it closed on its own —
-- the idle TTL, disk pressure, its owner — and those close here the same way.
-- A closed session takes no new run.
ALTER TABLE sessions ADD COLUMN close_requested_at INTEGER;
ALTER TABLE sessions ADD COLUMN closed_at INTEGER;
-- protocol/v1 SessionCloseReason: closed | closed_by_owner | expired | disk_pressure.
ALTER TABLE sessions ADD COLUMN close_reason TEXT;
