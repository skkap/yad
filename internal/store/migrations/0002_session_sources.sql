-- The sources a session's workdir was built from, as JSON, recorded once its
-- first preparation succeeds (decision 0033). A continuing run that names
-- none is prepared from these — its path sources locked again, the harness
-- started where the session's conversation lives — and one naming others is
-- refused: the workdir is already theirs.
ALTER TABLE sessions ADD COLUMN sources TEXT;
