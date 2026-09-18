-- When the service API asked a runner to drain (decision 0029). The drain
-- control goes out in every answer to its syncs until one says it is
-- draining, and the request is cleared then: nothing acknowledges a control,
-- and a lost response must not lose the drain. Cleared, it cannot drain the
-- runner's next process as well.
ALTER TABLE runners ADD COLUMN drain_requested_at INTEGER;
