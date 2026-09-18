---
date: 2026-09-18
---

# A curated set of dependencies, each one recorded

The runner holds durable state that must survive a crash in order — the session
map, the event spool, the result outbox — and that is what a database is for, not
a directory of JSON files. So the "standard library only" rule is replaced by a
curated list, each entry recorded in `ARCHITECTURE.md` with its reason:

- `modernc.org/sqlite` — pure-Go SQLite, so `CGO_ENABLED=0` cross-compilation
  keeps working (`mattn/go-sqlite3` needs cgo). WAL mode, one file per profile.
- `sqlc` — typed Go from SQL at build time; a tool, not a runtime dependency.
- `github.com/pelletier/go-toml/v2` — the owner edits `config.toml` by hand.
- `github.com/danielgtaylor/huma/v2` — typed HTTP operations for `yad hub` and
  the OpenAPI document generated from them.

Adding a dependency is still a reviewed change with a line in the list.

## Considered options

**Standard library only, JSON everywhere** — zero audit burden, and crash-safe
ordering hand-built. **`ncruces/go-sqlite3`** (wasm) — also cgo-free and a
smaller Go surface; younger and slower on writes.
