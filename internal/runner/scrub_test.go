package runner

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/workdir"
)

// The credential a version before decision 0068 wrote into state.db leaves
// the file itself, not only its rows — and so it does when the start that
// rewrote the rows was killed before the checkpoint that carries the new
// pages into the file: the next start has nothing left to rewrite, and
// checkpoints anyway.
func TestAStoredSourceCredentialLeavesStateDB(t *testing.T) {
	const token = "ghp_FAKEt0kenFAKEt0ken"
	url := "https://" + token + "@example.invalid/acme.git"
	for _, tc := range []struct {
		name string
		// killed rewrites the rows as the sweep does and commits, then
		// stops short of the checkpoint.
		killed bool
	}{{"one start", false}, {"a start killed before its checkpoint", true}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			db := e.store.DB
			for _, q := range []string{
				`INSERT INTO sessions (connection, id, harness, workdir, created_at, last_used_at, sources)
				 VALUES ('hub', 's1', 'claude', '/w', 1, 1, '[{"git":{"url":"` + url + `"}}]')`,
				`INSERT INTO runs (connection, id, session_id, harness, state, spec, created_at, updated_at)
				 VALUES ('hub', 'r1', 's1', 'claude', 'succeeded', '{"run_id":"r1","sources":[{"git":{"url":"` + url + `"}}]}', 1, 1)`,
				`INSERT INTO events (connection, run_id, seq, body) VALUES ('hub', 'r1', 1, '{"status":"fetching ` + url + `"}')`,
				`PRAGMA wal_checkpoint(TRUNCATE)`,
			} {
				if _, err := db.ExecContext(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			if !fileHolds(t, e.paths.StateDB(), token) {
				t.Fatal("the fixture's state.db does not hold the token")
			}
			if tc.killed {
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				for _, q := range []string{
					`UPDATE runs SET spec = replace(spec, '` + token + `@', ''), had_grants = 1`,
					`UPDATE sessions SET sources = replace(sources, '` + token + `@', '')`,
					`UPDATE events SET body = replace(body, '` + token + `@', '')`,
					`DELETE FROM start_sweeps`,
				} {
					if _, err := tx.ExecContext(ctx, q); err != nil {
						t.Fatalf("%s: %v", q, err)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}

			w := &workdir.Manager{Data: e.paths.Data, Slots: e.store}
			scrubSourceCredentials(ctx, e.store, w, slog.New(slog.DiscardHandler))

			for _, f := range []string{e.paths.StateDB(), e.paths.StateDB() + "-wal"} {
				if fileHolds(t, f, token) {
					t.Errorf("%s still holds the token", f)
				}
			}
			r, err := e.store.GetRun(ctx, dbRun("hub", "r1"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(r.Spec, "https://example.invalid/acme.git") || r.HadGrants == 0 {
				t.Errorf("run r1 is stored as %s, had_grants %d", r.Spec, r.HadGrants)
			}
			if pending, err := e.store.StartSweepPending(ctx, sourceCredentialsSweep); err != nil || pending {
				t.Errorf("the sweep is still owed (%v)", err)
			}
		})
	}
}

func fileHolds(t *testing.T, path, s string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(b), s)
}
