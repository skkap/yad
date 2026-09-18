// Package runner is the runner core: one connection per hub, the sync loop, the
// shared capacity pool with per-connection and per-harness caps, the executor
// that turns a claimed run into an adapter turn, the event spool uploader and
// the result outbox (ARCHITECTURE.md §2–§3).
//
// The single-hub path is epic E2; restart safety and drain are E3; many hubs
// sharing capacity are E7 (Zumino yad/dev).
package runner
