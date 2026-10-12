-- Runs whose harness said nothing at all before the first-output timeout
-- stopped them (harness_silent, decision 0075), counted per home: an account's,
-- or the harness's own default login, whose label is ''. Three in a row with
-- no turn on that home producing an event in between make the home not ready
-- until `until` (decision 0077), so a hub stops sending it work it cannot do.
--
-- Its own table rather than columns on accounts: a harness with no accounts
-- has no row there, and its default login is the home YAD-170 found wedged.
--
-- until is the authority, as limited_until is for a limit: the home is
-- offered again the moment it passes, and nothing has to come along and
-- write that down. runs is kept past it, so one more silent run after the
-- cool-down trips the home again at once; a turn that produces an event
-- deletes the row.
CREATE TABLE harness_silence (
    harness    TEXT NOT NULL,
    label      TEXT NOT NULL,
    runs       INTEGER NOT NULL,
    until      INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (harness, label)
);
