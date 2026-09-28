-- A session opened as a fork of another (decision 0065): its conversation
-- starts from a copy of that session's, so it can open only on the runner
-- holding that one. OfferCandidates offers a run opening it to that runner
-- alone, and a runner that goes away closes it with its own sessions, since
-- no other runner could ever open it. Once a claim binds it, it is a session
-- like any other and this column is history.
ALTER TABLE sessions ADD COLUMN fork_from TEXT REFERENCES sessions (id);
