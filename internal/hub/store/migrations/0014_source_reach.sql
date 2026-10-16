-- Source probing (decision 0088): what each runner advertising source_probe
-- last said about each repository this hub lists for it in probe_sources.
-- One row per runner and URL, replaced by a newer answer and dropped once
-- the hub stops listing the URL. Routing reads it: a run opening a session
-- on a repository a runner says it cannot reach is offered to another.
CREATE TABLE runner_source_reach (
    runner_id   TEXT NOT NULL REFERENCES runners (id),
    url         TEXT NOT NULL,
    reachable   INTEGER NOT NULL,
    -- The runner's class, protocol/v1 SourceReachError: '' when reachable.
    error       TEXT NOT NULL DEFAULT '',
    checked_at  INTEGER NOT NULL,
    received_at INTEGER NOT NULL,
    PRIMARY KEY (runner_id, url)
);
