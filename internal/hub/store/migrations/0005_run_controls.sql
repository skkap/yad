-- What the service API asked of a run a runner holds: cancel, interrupt or
-- steer, delivered as controls in that runner's syncs. A cancel and an
-- interrupt are sent on every sync until the run ends, since nothing in the
-- protocol acknowledges a control and a lost sync response must not lose a
-- cancel; each is kept once per run. A steer carries text the harness takes
-- once, so it is sent in one response and deleted (decision 0025).
CREATE TABLE run_controls (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     TEXT NOT NULL REFERENCES runs (id),
    kind       TEXT NOT NULL CHECK (kind IN ('cancel', 'interrupt', 'steer')),
    text       TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX run_controls_by_run ON run_controls (run_id, id);
