// Package account holds a runner's accounts per harness: each account's harness
// home, the transcript directory they share, usage-limit state with reset times,
// and owner-ordered failover (ARCHITECTURE.md §3, decision 0013).
//
// Epic E6 (Zumino yad/dev). Until then every run uses the harness's default home.
package account
