-- How far a run's events are contiguous from seq 1: the acked_through every
-- events call answers. Kept on the run so an upload advances it from where it
-- stood instead of rescanning the whole stream; a batch that leaves a gap is
-- stored and not acknowledged past it, so the runner resends from the gap.
ALTER TABLE runs ADD COLUMN events_through INTEGER NOT NULL DEFAULT 0;
