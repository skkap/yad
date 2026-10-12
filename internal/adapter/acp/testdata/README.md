# ACP fixtures

`acp-schema-1.23.0/` is the Agent Client Protocol schema the core is pinned
to, as released at `agentclientprotocol/agent-client-protocol` v1.23.0
(2026-09-18): `schema.unstable.json` and `meta.unstable.json`, the unstable
bundle, because `session/fork` and a prompt's `usage` are in it and not yet in
the stable one. `schema_test.go` hashes the core's surface of it against
`schema.go`'s pin (decisions 0067, 0073).

`agent/` holds conversations written by hand for what no recorded agent
shows: an agent speaking protocol version 2, one that resumes and forks
nothing, and one whose turn stops at the model's output limit with a
subagent's updates and a filesystem request on the same pipe, errors
answered with a null id (`*-null-id`), and answers the core cannot use: a
session opened with no id (`new-no-session`), a session/new refused
(`new-refused`) and a fork answered with the session it was to fork
(`fork-same-session`). OpenCode's
recorded runs are `internal/adapter/opencode/testdata`.
