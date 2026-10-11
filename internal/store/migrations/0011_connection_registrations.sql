-- Which registration the rows under a connection name belong to (decision
-- 0078). Everything here is keyed by (connection name, hub-chosen id), and a
-- name comes free again once `yad disconnect` has ended what it left (0072):
-- a different hub connected under it would meet the old hub's sessions and
-- runs, refused as a closed session or colliding with a kept run, whenever it
-- picks an id the old one used.
--
-- registered_at is the moment `yad connect` wrote the connection into
-- config.toml, copied from there. The CLI never writes this database (0043),
-- so the daemon compares the two at its start: a name whose config.toml entry
-- carries another registered_at is a new registration, and the rows still
-- under the name are moved to a retired name, <name>~<unix ms>, in one
-- transaction before its sync loop starts. 0 is a connection made before
-- config.toml carried the field, adopted as it stands.
CREATE TABLE connection_registrations (
    connection    TEXT PRIMARY KEY,
    registered_at INTEGER NOT NULL
);
