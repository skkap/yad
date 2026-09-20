-- What a run needs in order to be parked on a usage limit and picked up again
-- by a later sweep -- or by a later process (decision 0013).
--
-- A waiting run holds no process and no goroutine: the run's own goroutine
-- ends, and the resumer rebuilds the whole thing from this row. That is why
-- the state below is on disk rather than in memory: a restart is not a second
-- path that has to be kept in step with the first, it is the same path.

-- When the run first reached preparing, so a run that waited between two turns
-- reports how long the whole thing took rather than how long its last attempt
-- did. Null for rows written before this migration, and for a claim that has
-- not started; the executor falls back to its own clock then.
ALTER TABLE runs ADD COLUMN started_at INTEGER;

-- Every millisecond this run has spent waiting, summed over its parks. It is
-- what the result reports as waited_ms, and what the hub's max_wait_ms is
-- judged against, so it has to survive the restart the wait is meant to
-- survive.
ALTER TABLE runs ADD COLUMN waited_ms INTEGER NOT NULL DEFAULT 0;

-- When the current park began; null when the run is not waiting. The wait in
-- progress is not in waited_ms until it ends, so the two together are the
-- whole of it and neither double-counts.
ALTER TABLE runs ADD COLUMN waiting_since INTEGER;

-- How many times this run moved to another account. Reported as
-- account_switches, and persisted for the same reason waited_ms is.
ALTER TABLE runs ADD COLUMN account_switches INTEGER NOT NULL DEFAULT 0;

-- Whether the offer carried grants -- never how many, never their names. A
-- grant's name says as much about what a hub sent as its value, and the only
-- question this column answers is whether the run can be rebuilt at all: a
-- run's grants live in the process that claimed them and deliberately never
-- touch disk, so a parked run reconstructed by a later process has lost them.
-- Without this the row could not say that, and the run would silently start
-- without the secrets it was given.
ALTER TABLE runs ADD COLUMN had_grants INTEGER NOT NULL DEFAULT 0;
