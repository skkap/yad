-- Each account's usage windows: how much of each is used and when it refills,
-- as the harness last reported them (decision 0039, "limits are reported").
-- Claude names its windows five_hour and seven_day, Codex primary and
-- secondary; the names are each harness's own, as DOMAIN.md's definition of a
-- usage limit already is, so this table stores whatever the harness said.
--
-- A row per window rather than a blob on the account: every write is an upsert
-- of one window, which two runs of the same harness on the same account can do
-- at once without reading each other's work. Capacity is a shared pool and
-- nothing reserves an account, so concurrent turns on one account are ordinary
-- — and a read-modify-write of a whole snapshot would lose one of them.
--
-- Nothing here decides whether an account can run: that is the account's state
-- and its limited_until. These are for the hub to see why a runner will stop
-- claiming soon, and they survive the limit they explain.
CREATE TABLE account_windows (
    harness      TEXT NOT NULL,
    label        TEXT NOT NULL,
    name         TEXT NOT NULL,
    -- 0-100, whatever scale the harness reported: Codex gives a percentage,
    -- Claude a 0-1 fraction, and the adapters convert before this.
    used_percent REAL NOT NULL,
    -- Null when the harness gave the window's use without saying when it
    -- refills, which is not the same as refilling at the epoch.
    resets_at    INTEGER,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (harness, label, name)
);
