// Package workdir builds a run's workdir from its sources: a bare cache per
// repository, a worktree per session, local-path sources under a per-path lock,
// the repository's own .worktree/setup hook with the WT_* contract, and WT_SLOT
// allocation (ARCHITECTURE.md §3, decisions 0033 and 0034).
//
// Collection on close, idle TTL and disk pressure (decision 0011) is epic E4's
// DEV-18, which calls Manager.Reclaim.
package workdir
