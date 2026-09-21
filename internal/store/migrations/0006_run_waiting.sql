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
-- account_switches, and persisted for the same reason waited_ms is. Written
-- with the account at each move, so a run lost after a move still counts it.
ALTER TABLE runs ADD COLUMN account_switches INTEGER NOT NULL DEFAULT 0;

-- Whether the offer carried grants -- never how many, never their names. A
-- grant's name says as much about what a hub sent as its value, and the only
-- question this column answers is whether the run can be rebuilt at all: a
-- run's grants live in the process that claimed them and deliberately never
-- touch disk, so a parked run reconstructed by a later process has lost them.
-- Without this the row could not say that, and the run would silently start
-- without the secrets it was given.
ALTER TABLE runs ADD COLUMN had_grants INTEGER NOT NULL DEFAULT 0;

-- What the run's earlier turns cost, as JSON: usage per model, tool calls,
-- API retries, stalls, the first event's offset and the milliseconds already
-- spent with a process. A run is one run however many accounts and however
-- many processes it took, and protocol/v1's Result says its usage covers the
-- whole of it -- so a turn that ran before a park must still be in the total
-- the hub is finally told, and the hub's wall_clock_ms must not start again
-- at zero on the other side. Written as each turn ends rather than only at a
-- park, because a restart reports a run it finds lost from this row too
-- (decision 0030), and a run that moved accounts in process and was lost
-- before it parked would otherwise report none of what it spent. Null means
-- no turn of the run has ended.
ALTER TABLE runs ADD COLUMN spent TEXT;
