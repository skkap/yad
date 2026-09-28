-- The session this one was opened as a fork of, by its id on the same
-- connection (decision 0065). NULL for a session that started its own
-- conversation, which is every session before this column.
--
-- Kept on the session and not only in the run that opened it: until the
-- harness has given the fork a native id of its own, every run in it -- the
-- first, or the next after a first that failed before its harness started --
-- opens it from the forked session's conversation, and a later run carries
-- no fork_from of its own. Once the native id is set this column is history.
ALTER TABLE sessions ADD COLUMN fork_from TEXT;
