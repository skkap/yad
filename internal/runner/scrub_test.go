package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/logfile"
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
				// A run whose brief, not its sources, quotes the URL: its
				// spec changes too, so it is no longer the run as sent.
				`INSERT INTO runs (connection, id, session_id, harness, state, spec, created_at, updated_at)
				 VALUES ('hub', 'r2', 's1', 'claude', 'succeeded', '{"run_id":"r2","brief":{"instruction":"clone ` + url + `"}}', 2, 2)`,
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
			scrubSourceCredentials(ctx, e.store, w, nil, slog.New(slog.DiscardHandler))

			for _, f := range []string{e.paths.StateDB(), e.paths.StateDB() + "-wal"} {
				if fileHolds(t, f, token) {
					t.Errorf("%s still holds the token", f)
				}
			}
			for _, id := range []string{"r1", "r2"} {
				r, err := e.store.GetRun(ctx, dbRun("hub", id))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(r.Spec, "https://example.invalid/acme.git") || r.HadGrants == 0 {
					t.Errorf("run %s is stored as %s, had_grants %d", id, r.Spec, r.HadGrants)
				}
			}
			if pending, err := e.store.StartSweepPending(ctx, sourceCredentialsSweep); err != nil || pending {
				t.Errorf("the sweep is still owed (%v)", err)
			}
		})
	}
}

// The credential a version before decision 0068 wrote into the daemon's log
// leaves the live file and its backups at the daemon's start, and the log goes
// on. A start whose log could not be rewritten leaves state.db as it was and
// the sweep still owed: its rows are how the next start finds the credential
// for the log.
func TestADaemonStartTakesALoggedSourceCredentialOut(t *testing.T) {
	const token = "ghp_FAKEl0gT0kenFAKEl0g"
	url := "https://x-access-token:" + token + "@example.invalid/acme.git"
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.store.DB.ExecContext(ctx,
		`INSERT INTO sessions (connection, id, harness, workdir, created_at, last_used_at, sources)
		 VALUES ('hub', 's1', 'claude', '/w', 1, 1, '[{"git":{"url":"`+url+`"}}]')`); err != nil {
		t.Fatal(err)
	}
	// Lines as v0.1.0 wrote them: slog's JSON, the URL quoted whole.
	planted := func(msg string) []byte {
		var b bytes.Buffer
		slog.New(slog.NewJSONHandler(&b, nil)).Warn(msg, "err", "base main is not a branch, tag or commit of "+url)
		return b.Bytes()
	}
	live, backup := e.paths.Log(), logfile.Backup(e.paths.Log(), 1)
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{live, backup} {
		if err := os.WriteFile(f, planted("the workdir could not be prepared"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	start := func(scrubLog func(...string) ([]string, error), log *slog.Logger) {
		t.Helper()
		if err := Serve(ctx, Options{
			Paths: e.paths, Config: config.Default(), RunnerID: "r",
			Capabilities: func() v1.Capabilities { return drivableDoc("r", 1) },
			Log:          log, ScrubLog: scrubLog, Drain: drained(),
		}); err != nil {
			t.Fatalf("Serve: %v", err)
		}
	}

	start(func(...string) ([]string, error) { return nil, errors.New("the disk is full") }, slog.New(slog.DiscardHandler))
	if pending, err := e.store.StartSweepPending(ctx, sourceCredentialsSweep); err != nil || !pending {
		t.Fatalf("a start whose log could not be rewritten marked the sweep done (pending %v, %v)", pending, err)
	}
	var sources string
	if err := e.store.DB.QueryRowContext(ctx, `SELECT sources FROM sessions WHERE id = 's1'`).Scan(&sources); err != nil || !strings.Contains(sources, token) {
		t.Fatalf("a start whose log could not be rewritten took the credential out of state.db, where the next start finds it (%v)", err)
	}

	logf, err := logfile.Open(live, logfile.DefaultMaxBytes, logfile.DefaultBackups)
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()
	log := slog.New(slog.NewJSONHandler(logf, nil))
	start(logf.Scrub, log)
	log.Info("a line after the start")

	for _, f := range []string{live, backup} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), token) {
			t.Errorf("%s still holds the token", f)
		}
		if !strings.Contains(string(b), "commit of https://example.invalid/acme.git") {
			t.Errorf("%s lost the line that quoted the URL rather than having the credential taken out of it", f)
		}
		if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v after the rewrite (%v); want 0600", f, fi.Mode(), err)
		}
	}
	b, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"took out the credentials", "a line after the start"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the live log does not go on after the rewrite: no %q in\n%s", want, b)
		}
	}
	if pending, err := e.store.StartSweepPending(ctx, sourceCredentialsSweep); err != nil || pending {
		t.Errorf("the sweep is still owed (%v)", err)
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
