// Package workdir builds and reclaims session workdirs: bare caches per
// repository, a worktree per session, local-path sources under a per-path lock,
// the repository's own .worktree/setup hook with the WT_* contract, WT_SLOT
// allocation, and collection on close, idle TTL and disk pressure
// (ARCHITECTURE.md §3, decision 0011).
//
// Epic E4 (Zumino yad/dev). E2 uses a plain empty directory per session.
package workdir
