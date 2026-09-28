-- A hub adding and removing a runner's accounts (decision 0057).
--
-- add_account marks a login that creates its account: start_login or
-- login_token carries add, and the runner lists the account once the login
-- takes.
ALTER TABLE logins ADD COLUMN add_account INTEGER NOT NULL DEFAULT 0;

-- A removal the service API asked for. remove_account goes out in every
-- answer to the runner's syncs until its health leaves the account out, or
-- the runner no longer advertises accounts; the row goes then. Nothing
-- acknowledges a control, and a lost response must not lose the removal.
CREATE TABLE account_removals (
    runner_id    TEXT NOT NULL REFERENCES runners (id),
    harness      TEXT NOT NULL,
    account      TEXT NOT NULL,
    requested_at INTEGER NOT NULL,
    PRIMARY KEY (runner_id, harness, account)
);
