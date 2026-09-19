-- An account's state as DOMAIN.md names it: free, limited until a reset, or
-- needs login (decisions 0013 and 0039). The 0001 table held only
-- limited_until, which cannot tell a working account from one whose login the
-- owner never finished — and needs-login has to be skipped for claiming and
-- reported to every hub, so it has to be a fact of its own.
--
-- Default 'free': a row written by an older yad, and a label the owner adds to
-- config.toml by hand without running `yad account add`, are both usable until
-- something says otherwise. Absence is data — an account with no row is free.
--
-- limited_until stays where it is and stays DEV-27's to write; nothing here
-- sets 'limited'.
ALTER TABLE accounts ADD COLUMN state TEXT NOT NULL DEFAULT 'free'
    CHECK (state IN ('free', 'limited', 'needs_login'));

-- When the state last moved, so `yad account list` can say how old a
-- needs-login is without the owner guessing.
ALTER TABLE accounts ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
