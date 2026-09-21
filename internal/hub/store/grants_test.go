package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// withGrants is a run carrying two grants, in an order a rebuilt list must
// keep: a reordered list is a different spec to anyone comparing one.
func withGrants(id string) v1.Run {
	r := run(id, "s-"+id)
	r.Grants = []v1.Grant{
		{Name: "ZUMINO_TOKEN", Value: "zum-secret-" + id, As: v1.GrantEnv},
		{Name: "DEPLOY_KEY", Value: "key-secret-" + id, As: v1.GrantFile},
	}
	return r
}

// setState moves a run with a bare UPDATE, on purpose: the rule belongs to the
// schema, so a terminal path nobody has written yet is held to it too.
func setState(t *testing.T, s *Store, id, state string) {
	t.Helper()
	if _, err := s.DB.ExecContext(context.Background(), `UPDATE runs SET state = ? WHERE id = ?`, state, id); err != nil {
		t.Fatal(err)
	}
}

func storedSpec(t *testing.T, s *Store, id string) (string, v1.Run) {
	t.Helper()
	stored, err := s.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var r v1.Run
	if err := json.Unmarshal([]byte(stored.Spec), &r); err != nil {
		t.Fatalf("stored spec is not a run: %v\n%s", err, stored.Spec)
	}
	return stored.Spec, r
}

// A grant's value is held only while its run can still use it (decision
// 0041). Every terminal state blanks the values and keeps each grant's name
// and delivery; every state a run can still be offered, started or resumed
// from keeps them — waiting included, since its resume is built from the spec.
func TestTerminalRunsForgetGrantValues(t *testing.T) {
	s, _ := open(t)
	for _, tc := range []struct {
		state  string
		forget bool
	}{
		{"queued", false},
		{"offered", false},
		{"claimed", false},
		{"preparing", false},
		{"running", false},
		{"waiting", false},
		{"succeeded", true},
		{"failed", true},
		{"cancelled", true},
		{"timed_out", true},
		{"lost", true},
	} {
		t.Run(tc.state, func(t *testing.T) {
			want := withGrants("r-" + tc.state)
			if err := s.EnqueueRun(context.Background(), want, t0); err != nil {
				t.Fatal(err)
			}
			setState(t, s, want.RunID, tc.state)
			_, got := storedSpec(t, s, want.RunID)
			want.Session.Mode = v1.SessionPerRun // EnqueueRun fills the default in
			if tc.forget {
				for i := range want.Grants {
					want.Grants[i].Value = ""
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("spec after %s:\n got %+v\nwant %+v", tc.state, got, want)
			}
		})
	}
}

// A run with no grants ends with its spec byte for byte as it was: the
// trigger does not add an empty list to a spec that had none.
func TestAnEndedRunWithoutGrantsKeepsItsSpec(t *testing.T) {
	s, _ := open(t)
	r := run("plain", "s-plain")
	if err := s.EnqueueRun(context.Background(), r, t0); err != nil {
		t.Fatal(err)
	}
	before, _ := storedSpec(t, s, r.RunID)
	setState(t, s, r.RunID, "succeeded")
	if after, _ := storedSpec(t, s, r.RunID); after != before {
		t.Errorf("spec changed:\n was %s\n now %s", before, after)
	}
}

// Blanking the column is not enough if the old bytes are still in the file:
// hub.db is what an owner copies, backs up or leaves readable, and a value
// SQLite freed without overwriting is still there to read. Once the runs have
// ended and the store has closed — checkpointing its -wal back in — no byte
// of any value is anywhere on disk.
//
// Hundreds of runs with briefs of varying length, because one small run
// proves nothing: its page is rewritten whole. Rows spread over many pages
// are moved when they shrink, and without secure_delete a few values in every
// few hundred survived in free blocks — two of 300 when this was measured.
func TestAnEndedRunsGrantValuesLeaveTheFile(t *testing.T) {
	s, file := open(t)
	const runs = 300
	var values []string
	for i := range runs {
		r := withGrants(fmt.Sprintf("gone-%03d", i))
		r.Brief.Instruction = strings.Repeat("x", 1500+i)
		for _, g := range r.Grants {
			values = append(values, g.Value)
		}
		if err := s.EnqueueRun(context.Background(), r, t0); err != nil {
			t.Fatal(err)
		}
	}
	for i := range runs {
		setState(t, s, fmt.Sprintf("gone-%03d", i), "succeeded")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, file + "-wal"} {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		left := 0
		for _, v := range values {
			if bytes.Contains(b, []byte(v)) {
				left++
			}
		}
		if left > 0 {
			t.Errorf("%s still holds %d of %d grant values", filepath.Base(path), left, len(values))
		}
	}
}
