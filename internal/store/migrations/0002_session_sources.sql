-- The sources a session's first run named, as JSON — [] for none — recorded
-- whatever became of that run, unless its sources were refused (decision
-- 0033). NULL is a session from before this column, bound by its next run. A continuing run that names
-- none is prepared from these — its path sources locked again, the harness
-- started where the session's conversation lives — and one naming others is
-- refused: the workdir is already theirs.
ALTER TABLE sessions ADD COLUMN sources TEXT;
