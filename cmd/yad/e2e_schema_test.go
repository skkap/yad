package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/store"
)

// The start that follows an upgrade, YAD-196: state.db is still at the schema
// the release before wrote, and an account in it is limited. The first
// capability document — the one a hub is sent at the first sync — says so,
// and the start logs no warning that it could not read the account states,
// nor the restart that is already under way as the way out.
func TestTheFirstDocumentAfterAnUpgradeReadsTheAccountStates(t *testing.T) {
	latest := latestMigration(t)
	for _, tc := range []struct {
		name   string
		schema int
	}{
		{"one migration behind", latest - 1},
		// 5 is the first schema with account_windows.
		{"several migrations behind", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, claudeE2E)
			m.listAccounts("claude", "work")
			p := config.Paths{Config: m.p.config, Data: m.p.data}
			if err := os.MkdirAll(account.HomeDir(m.p.data, "claude", "work"), 0o700); err != nil {
				t.Fatal(err)
			}
			resets := time.Now().Add(3 * time.Hour).Truncate(time.Millisecond)
			olderStateDB(t, p.StateDB(), tc.schema, resets)

			// An hour: the document the hub holds after the first sync is
			// the first one built, with no probe after it to correct it.
			d := m.daemon("--interval", "1h")
			id := m.runnerID()
			var doc v1.Capabilities
			eventually(t, "the daemon has synced", func() bool {
				r, err := m.hubDB.GetRunner(context.Background(), id)
				return err == nil && r.LastSyncAt.Valid && json.Unmarshal([]byte(r.Capabilities), &doc) == nil
			})
			var work *v1.AccountReport
			for _, h := range doc.Harnesses {
				if h.ID != "claude" {
					continue
				}
				if i := slices.IndexFunc(h.Accounts, func(a v1.AccountReport) bool { return a.Label == "work" }); i >= 0 {
					work = &h.Accounts[i]
				}
			}
			switch {
			case work == nil:
				t.Errorf("the first document has no claude account work:\n%+v", doc.Harnesses)
			case work.State != v1.AccountLimited:
				t.Errorf("the first document reports work %s, want %s (limited until %s)", work.State, v1.AccountLimited, resets.UTC())
			}

			d.halt(t)
			// The document's own warning, by its words: a sync cut short by
			// the halt can say it could not read the states too, for that
			// reason and not this one.
			if out := d.out.String(); strings.Contains(out, "reporting the owner's accounts as free") || strings.Contains(out, "has not migrated it yet") {
				t.Errorf("the start warned that it could not read the account states:\n%s", out)
			}
		})
	}
}

// latestMigration is the schema this yad migrates state.db to.
func latestMigration(t *testing.T) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "store", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found (%v)", err)
	}
	slices.Sort(files)
	n, err := strconv.Atoi(strings.SplitN(filepath.Base(files[len(files)-1]), "_", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// olderStateDB writes state.db as a release whose last migration was schema
// left it, opened the way the daemon opens it, with the claude account work
// limited until resets: its state row and a spent window.
func olderStateDB(t *testing.T, file string, schema int, resets time.Time) {
	t.Helper()
	dir := filepath.Join("..", "..", "internal", "store", "migrations")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{}
	for _, f := range files {
		n, err := strconv.Atoi(strings.SplitN(filepath.Base(f), "_", 2)[0])
		if err != nil {
			t.Fatal(err)
		}
		if n > schema {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fsys["migrations/"+filepath.Base(f)] = &fstest.MapFile{Data: b}
	}
	ctx := context.Background()
	conn, err := store.OpenSQLite(ctx, file, fsys)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	now := time.Now().UnixMilli()
	if _, err := conn.ExecContext(ctx, `INSERT INTO accounts (harness, label, limited_until, state, updated_at) VALUES ('claude', 'work', ?, 'limited', ?)`, resets.UnixMilli(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO account_windows (harness, label, name, used_percent, resets_at, updated_at) VALUES ('claude', 'work', 'five_hour', 100, ?, ?)`, resets.UnixMilli(), now); err != nil {
		t.Fatal(err)
	}
	var got int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&got); err != nil || got != schema {
		t.Fatalf("state.db is at schema %d (%v), want %d", got, err, schema)
	}
}
