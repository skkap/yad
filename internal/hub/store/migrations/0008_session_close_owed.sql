-- Set when the hub closed a session because its runner had gone silent
-- (decision 0046). The runner may come back, and its workdir is still on its
-- disk: close_session goes to it until it reports the close, which clears
-- this. A session closed any other way never sets it.
ALTER TABLE sessions ADD COLUMN close_owed INTEGER NOT NULL DEFAULT 0;
