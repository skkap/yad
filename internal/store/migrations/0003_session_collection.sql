-- Workdir collection (decision 0035). A session closes — by its hub's word,
-- its owner's, the idle TTL or disk pressure — in one step that also checks no
-- run is held in it; its workdir is reclaimed after, and a removal that fails
-- is tried again, so closed and reclaimed are separate facts. Its hub is told
-- in every sync until one carrying it is answered.
--
-- 0002 was set aside for the git sources work (DEV-16/17), which needed none,
-- and is retired: migrate applies only numbers above a database's schema
-- version, so a 0002 written after this one would be skipped on every database
-- already at 3. TestMigrationNumbering holds that line.

-- Why the session closed: closed | closed_by_owner | expired | disk_pressure
-- (protocol/v1 SessionCloseReason). Set with close_requested_at while a run
-- held in the session keeps it open, and kept once it closes.
ALTER TABLE sessions ADD COLUMN close_reason TEXT;
-- A close asked for while a run was held: the session closes when it ends.
ALTER TABLE sessions ADD COLUMN close_requested_at INTEGER;
ALTER TABLE sessions ADD COLUMN closed_at INTEGER;
-- The workdir is gone, and the git worktrees and WT_SLOTs it held with it.
ALTER TABLE sessions ADD COLUMN reclaimed_at INTEGER;
-- A sync carrying the close was answered by the session's hub.
ALTER TABLE sessions ADD COLUMN reported_at INTEGER;
