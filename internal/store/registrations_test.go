package store

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/store/db"
)

// seedAll is one connection's rows in every table keyed by it: a session
// holding a WT_SLOT, a run in it, an event and a result owed.
func seedAll(t *testing.T, s *Store, conn string) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateSession(ctx, db.CreateSessionParams{ID: "s1", Connection: conn, Harness: "claude", Workdir: "/w/" + conn + "/s1", CreatedAt: 1, LastUsedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, db.CreateRunParams{ID: "r1", SessionID: "s1", Connection: conn, Harness: "claude", Spec: "{}", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, db.AppendEventParams{Connection: conn, RunID: "r1", Seq: 1, Body: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutOutbox(ctx, db.PutOutboxParams{Connection: conn, RunID: "r1", Body: "{}"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Slot(ctx, "/repos/a.git", conn, "s1"); err != nil {
		t.Fatal(err)
	}
}

// rowsUnder counts what each table keyed by connection holds under conn.
func rowsUnder(t *testing.T, s *Store, conn string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"sessions", "runs", "events", "outbox", "slots"} {
		var n int
		// The table names are the constants above.
		if err := s.DB.QueryRow("SELECT count(*) FROM "+table+" WHERE connection = ?", conn).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

func registration(t *testing.T, s *Store, conn string) (int64, bool) {
	t.Helper()
	at, err := s.ConnectionRegistration(context.Background(), conn)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return at, true
}

func allOne(m map[string]int) bool {
	for _, n := range m {
		if n != 1 {
			return false
		}
	}
	return true
}

func noneIn(m map[string]int) bool {
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

// A name given to a new registration has the earlier one's rows moved to a
// retired name, every table together, and the new hub may then use the ids
// the old one did (DEV-166).
func TestANewRegistrationRetiresTheEarlierOnesRows(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, err := s.AdoptRegistration(ctx, "hub", 100, time.UnixMilli(5000)); err != nil {
		t.Fatal(err)
	}
	seedAll(t, s, "hub")

	now := time.UnixMilli(9000)
	r, err := s.AdoptRegistration(ctx, "hub", 200, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := "hub~9000"; r.As != want || r.Sessions != 1 || r.Runs != 1 {
		t.Errorf("retirement %+v, want %s with one session and one run", r, want)
	}
	if err := config.ValidName(r.As); err == nil {
		t.Errorf("the retired name %q is one a connection may be given, so a configured connection could meet it", r.As)
	}
	if got := rowsUnder(t, s, "hub"); !noneIn(got) {
		t.Errorf("the reused name still holds %v", got)
	}
	if got := rowsUnder(t, s, r.As); !allOne(got) {
		t.Errorf("the retired name holds %v, want one row in each table", got)
	}
	if at, ok := registration(t, s, "hub"); !ok || at != 200 {
		t.Errorf("recorded registration %d (%v), want 200", at, ok)
	}
	var violations int
	if err := s.DB.QueryRow("SELECT count(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Errorf("%d rows name a parent that is not there", violations)
	}
	// The new hub reuses the old one's ids.
	seedAll(t, s, "hub")
}

// Nothing moves without a new registration: a restart, an upgrade from a
// version whose config.toml had no registered_at, and a name nobody used.
func TestAdoptionMovesNothingWithoutANewRegistration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before []int64 // registrations adopted before, in order; none for a database that predates the table
		now    int64
		rows   bool
	}{
		{"an upgrade: rows, and no registered_at", nil, 0, true},
		{"a restart under the same registration", []int64{200}, 200, true},
		{"a restart after an upgrade", []int64{0}, 0, true},
		{"registered_at lost from config.toml", []int64{200}, 0, true},
		{"a first connection under a fresh name", nil, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := open(t)
			ctx := context.Background()
			for _, at := range tc.before {
				if _, err := s.AdoptRegistration(ctx, "hub", at, time.UnixMilli(1)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.rows {
				seedAll(t, s, "hub")
			}
			r, err := s.AdoptRegistration(ctx, "hub", tc.now, time.UnixMilli(9000))
			if err != nil {
				t.Fatal(err)
			}
			if r.As != "" {
				t.Errorf("moved the rows to %s", r.As)
			}
			if got := rowsUnder(t, s, "hub"); tc.rows && !allOne(got) {
				t.Errorf("the name holds %v, want its rows where they were", got)
			}
			if at, ok := registration(t, s, "hub"); !ok || at != tc.now {
				t.Errorf("recorded registration %d (%v), want %d", at, ok, tc.now)
			}
		})
	}
}

// An upgrade adopts what is there; the first new registration after it
// retires those rows. Without the 0 recorded at the upgrade, the database
// could not tell them from the new hub's.
func TestTheFirstRegistrationAfterAnUpgradeRetiresTheOldRows(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	seedAll(t, s, "hub")
	if _, err := s.AdoptRegistration(ctx, "hub", 0, time.UnixMilli(1)); err != nil {
		t.Fatal(err)
	}
	r, err := s.AdoptRegistration(ctx, "hub", 300, time.UnixMilli(9000))
	if err != nil {
		t.Fatal(err)
	}
	if r.As != "hub~9000" || !noneIn(rowsUnder(t, s, "hub")) {
		t.Errorf("retirement %+v; the name still holds %v", r, rowsUnder(t, s, "hub"))
	}

	// Upgraded between the disconnect and the connect, no daemon of this
	// version saw the old connection, and nothing is recorded for the name.
	// Its rows still predate the registration config.toml names — only an
	// entry without registered_at could have written them — so they go.
	s2, _ := open(t)
	seedAll(t, s2, "hub")
	r, err = s2.AdoptRegistration(ctx, "hub", 300, time.UnixMilli(9000))
	if err != nil {
		t.Fatal(err)
	}
	if r.As != "hub~9000" || !noneIn(rowsUnder(t, s2, "hub")) {
		t.Errorf("rows from before the table: retirement %+v; the name still holds %v", r, rowsUnder(t, s2, "hub"))
	}
}

// A crash, or any failure, part way through leaves every row where it was and
// the earlier registration recorded: the next start tries the whole of it
// again.
func TestRetirementIsWholeOrNothing(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, err := s.AdoptRegistration(ctx, "hub", 100, time.UnixMilli(1)); err != nil {
		t.Fatal(err)
	}
	seedAll(t, s, "hub")
	// The last table renamed fails, after the others have moved.
	if _, err := s.DB.Exec(`CREATE TEMP TRIGGER fail_slots BEFORE UPDATE ON slots BEGIN SELECT RAISE(ABORT, 'disk gone'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdoptRegistration(ctx, "hub", 200, time.UnixMilli(9000)); err == nil {
		t.Fatal("the retirement succeeded with its last step failing")
	}
	if got := rowsUnder(t, s, "hub"); !allOne(got) {
		t.Errorf("after a failed retirement the name holds %v, want every row still there", got)
	}
	if got := rowsUnder(t, s, "hub~9000"); !noneIn(got) {
		t.Errorf("after a failed retirement the retired name holds %v", got)
	}
	if at, _ := registration(t, s, "hub"); at != 100 {
		t.Errorf("recorded registration %d after a failed retirement, want the earlier 100", at)
	}

	if _, err := s.DB.Exec(`DROP TRIGGER fail_slots`); err != nil {
		t.Fatal(err)
	}
	r, err := s.AdoptRegistration(ctx, "hub", 200, time.UnixMilli(9000))
	if err != nil || r.As != "hub~9000" {
		t.Fatalf("the retry: %+v, %v", r, err)
	}
}

// Two retirements of one name in the same millisecond do not share a name.
func TestRetiredNamesDoNotCollide(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	now := time.UnixMilli(9000)
	if _, err := s.AdoptRegistration(ctx, "hub", 100, now); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, at := range []int64{200, 300, 400} {
		seedAll(t, s, "hub")
		r, err := s.AdoptRegistration(ctx, "hub", at, now)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, r.As)
	}
	if want := []string{"hub~9000", "hub~9001", "hub~9002"}; !slices.Equal(names, want) {
		t.Errorf("retired as %v, want %v", names, want)
	}
}
