package runner

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
	"github.com/skkap/yad/internal/workdir"
)

// sourceCredentialsSweep names the start sweep 0009_start_sweeps.sql owes.
const sourceCredentialsSweep = "source_credentials"

// scrubSourceCredentials takes out of this machine what a version before
// decision 0068 kept of the credential an https source URL carried: the bare
// caches it made from such a URL, and every copy of the credential in
// state.db. It runs at every daemon start, before any run can be prepared, and
// finds nothing once it has run: the caches are looked for each time, since
// that is a directory listing, and state.db's rows are read once, when the
// migration that asks for it has just run.
//
// state.db first, one transaction, then the caches: the credential a cache
// holds is how its copies in the store are found, so a start that stops
// between the two finds the cache again and repeats both. Nothing here stops
// the daemon; what fails is logged, never with the credential, and tried
// again at the next start.
func scrubSourceCredentials(ctx context.Context, st *store.Store, w *workdir.Manager, log *slog.Logger) {
	caches, err := w.StaleCaches(ctx)
	if err != nil {
		log.Warn("a bare cache could not be checked for a credential an earlier version kept in its URL; the next start looks again", "err", err)
	}
	scrubs := map[workdir.Scrub]bool{}
	for _, c := range caches {
		scrubs[c.Scrub] = true
	}
	var rows int64
	err = st.Tx(ctx, func(q *db.Queries) error {
		pending, err := q.StartSweepPending(ctx, sourceCredentialsSweep)
		if err != nil {
			return err
		}
		if pending {
			specs, err := q.RunSpecsWithAt(ctx)
			if err != nil {
				return err
			}
			for _, r := range specs {
				var run struct {
					Sources []v1.Source `json:"sources"`
				}
				if json.Unmarshal([]byte(r.Spec), &run) != nil {
					continue
				}
				for _, s := range workdir.ScrubsOf(run.Sources) {
					scrubs[s] = true
				}
			}
			sessions, err := q.SessionSourcesWithAt(ctx)
			if err != nil {
				return err
			}
			for _, s := range sessions {
				var sources []v1.Source
				if json.Unmarshal([]byte(s.Sources.String), &sources) != nil {
					continue
				}
				for _, sc := range workdir.ScrubsOf(sources) {
					scrubs[sc] = true
				}
			}
		}
		for s := range scrubs {
			for _, f := range s.Forms() {
				n, err := scrubText(ctx, q, f)
				if err != nil {
					return err
				}
				rows += n
			}
		}
		if pending {
			return q.StartSweepDone(ctx, sourceCredentialsSweep)
		}
		return nil
	})
	if err != nil {
		log.Warn("the credentials an earlier version kept in source URLs could not be taken out of state.db; the next start tries again", "err", err)
		return
	}
	// The rewritten pages reach state.db itself at a checkpoint; until then
	// the old ones, credential and all, are still in it (secure_delete zeroes
	// what the update freed, in the new page). At every start, not only one
	// that rewrote something: a start killed between its commit and this line
	// leaves the next one nothing to rewrite and the old pages still in the
	// file. After a clean stop the WAL is empty and this costs nothing.
	var busy, frames, done int
	err = st.DB.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &done)
	if err == nil && busy != 0 {
		err = errors.New("a reader held the database")
	}
	if err != nil {
		log.Warn("state.db could not be checkpointed, so pages from before the credentials an earlier version kept in source URLs were taken out may still be in it; the next start tries again, and SQLite does as the daemon stops", "err", err)
	}
	var cleaned, renamed int
	for _, c := range caches {
		moved, err := w.CleanCache(ctx, c)
		if moved {
			renamed++
			// Only once the cache is at its new name: a session in it keeps
			// the WT_SLOT its setup hook derived ports from. A start stopped
			// before this line leaves the rows under the old name, and such a
			// session is given a slot afresh — its hook, done, is not run
			// again.
			if err := st.RenameSlotsRepo(ctx, db.RenameSlotsRepoParams{NewRepo: filepath.Base(c.Target), OldRepo: filepath.Base(c.Path)}); err != nil {
				log.Warn("the WT_SLOTs of a bare cache moved to its name without a credential could not follow it; its sessions are given slots afresh", "err", err)
			}
		}
		if err != nil {
			log.Warn("the credential an earlier version kept in a bare cache's URL could not be taken out; the next start tries again", "err", err)
			continue
		}
		cleaned++
	}
	if rows > 0 || cleaned > 0 {
		log.Info("took out the credentials an earlier version kept from the source URLs it stored (decision 0068)",
			"state_rows", rows, "caches", cleaned, "caches_renamed", renamed)
	}
}

// scrubText replaces s wherever state.db's text may quote a source URL: the
// run's spec and reason, the session's sources, the events and results kept
// for the hub.
func scrubText(ctx context.Context, q *db.Queries, s workdir.Scrub) (int64, error) {
	var total int64
	for _, scrub := range []func() (int64, error){
		func() (int64, error) { return q.ScrubRuns(ctx, db.ScrubRunsParams{Old: s.Old, New: s.New}) },
		func() (int64, error) { return q.ScrubSessions(ctx, db.ScrubSessionsParams{Old: s.Old, New: s.New}) },
		func() (int64, error) { return q.ScrubEvents(ctx, db.ScrubEventsParams{Old: s.Old, New: s.New}) },
		func() (int64, error) { return q.ScrubOutbox(ctx, db.ScrubOutboxParams{Old: s.Old, New: s.New}) },
	} {
		n, err := scrub()
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
