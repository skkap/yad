-- Admin tokens are what a service or a person presents to yad hub's service
-- API (protocol/hubapi) to submit runs and follow them. They are a separate
-- kind from runner credentials, in a separate table, so neither can ever be
-- accepted where the other is expected. Stored as SHA-256 hashes, like every
-- other secret here; the name is how a person tells them apart and revokes one.
CREATE TABLE admin_tokens (
    hash       TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);
