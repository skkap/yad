// Package workdir builds a run's workdir from its sources: a bare cache per
// repository, a worktree per session, local-path sources under a per-path lock,
// the repository's own .worktree/setup hook with the WT_* contract, and WT_SLOT
// allocation (ARCHITECTURE.md §3, decisions 0033 and 0034).
//
// Collection on close, idle TTL and disk pressure (decisions 0011 and 0035)
// is internal/runner's Collector, which calls Manager.Reclaim with the
// manager the executor prepares with, before it deletes a workdir.
package workdir
