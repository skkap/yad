-- Hub logins (decision 0055): an account's login this hub started on a
-- runner, by link or by token. The service API makes the row, and the
-- runner's syncs move it. state is 'requested' until the runner first
-- reports the login, and the runner's own word after that: starting,
-- waiting, checking, succeeded, failed, expired or cancelled. 'requested'
-- says only that no report has come: whether the control went out is
-- sent_at. The hub ends a login by its own act at once only while it was
-- never sent; one sent and not yet reported may already be the runner's.
CREATE TABLE logins (
    id                  TEXT PRIMARY KEY,
    runner_id           TEXT NOT NULL REFERENCES runners (id),
    harness             TEXT NOT NULL,
    -- The account label, or '' for the harness's own default login.
    account             TEXT NOT NULL DEFAULT '',
    method              TEXT NOT NULL CHECK (method IN ('link', 'token')),
    state               TEXT NOT NULL DEFAULT 'requested',
    -- What the runner reported: the harness's authorize link, a device
    -- code, and why it ended. Data, shown and never followed.
    url                 TEXT NOT NULL DEFAULT '',
    user_code           TEXT NOT NULL DEFAULT '',
    error               TEXT NOT NULL DEFAULT '',
    -- The owner's code, held until the runner reports the login past
    -- waiting (logins_forget_code).
    code                TEXT NOT NULL DEFAULT '',
    -- A token login's token, held only until a sync from the runner reports
    -- the login it was delivered for (logins_forget_token): a token works for
    -- a year, and a hub keeps it for one delivery, as it keeps a grant only
    -- while its run can use it (decision 0041). Never returned by the API.
    token               TEXT NOT NULL DEFAULT '',
    cancel_requested_at INTEGER,
    -- When a sync's answer first carried start_login or login_token for it.
    sent_at             INTEGER,
    -- 1 when the hub ended it by its own act rather than the runner's
    -- report, which a runner's own report of an end then replaces: the
    -- runner knows what happened on the machine, and the hub only guessed.
    hub_ended           INTEGER NOT NULL DEFAULT 0,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
);

CREATE INDEX logins_by_runner ON logins (runner_id, state);

-- Every write of state goes through here, whoever makes it: the runner's
-- report, or the hub ending a login itself. A trigger rather than each
-- query's own SET, so no later query can forget.
CREATE TRIGGER logins_forget_token AFTER UPDATE OF state ON logins
WHEN NEW.token <> '' AND NEW.state <> 'requested'
BEGIN
    UPDATE logins SET token = '' WHERE id = NEW.id;
END;

CREATE TRIGGER logins_forget_code AFTER UPDATE OF state ON logins
WHEN NEW.code <> '' AND NEW.state NOT IN ('requested', 'starting', 'waiting')
BEGIN
    UPDATE logins SET code = '' WHERE id = NEW.id;
END;
