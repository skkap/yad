package account

import (
	"context"
	"time"

	"github.com/skkap/yad/internal/store/db"
)

// SilentRuns is how many turns in a row on one home have to end harness_silent
// before the home is reported not ready (decision 0077). One silent start can
// be a slow one that ran past the first-output timeout once — MCP servers
// connecting, a SessionStart hook — and two can be two runs started together
// into one passing fault. Three in a row, with no turn on the home saying a
// word in between, is past coincidence; it is what DEV-170 saw, and it costs at
// most three two-minute failures before hubs are told.
const SilentRuns = 3

// silentCoolDown is how long a home that reached SilentRuns is left alone, and
// silentCoolDownCap the most it is ever left. The cool-down doubles with each
// silent run past the third: a home is never told apart from a working one
// except by being tried, and a cause that stays — another Claude running in
// the home for good — would otherwise fail a hub's run every quarter of an
// hour for ever. Capped, so no home is ever more than four hours from its next
// chance, and nothing here can strand a runner: a not-ready home is offered no
// run with which to prove itself, so only the clock can bring it back.
const (
	silentCoolDown    = 15 * time.Minute
	silentCoolDownCap = 4 * time.Hour
)

// Silence is one home's record of runs whose harness said nothing at all
// before the first-output timeout stopped them. Label is the account's, or ""
// for the harness's own default login.
type Silence struct {
	Harness string
	Label   string
	// Runs is how many turns in a row ended harness_silent.
	Runs int
	// Until is when the home is offered again; zero while Runs is below
	// SilentRuns, or after a daemon start ended the cool-down.
	Until time.Time
	// At is when the last silent run was recorded.
	At time.Time
}

// Holds says whether the home is not ready at now.
func (s Silence) Holds(now time.Time) bool {
	return s.Runs >= SilentRuns && now.Before(s.Until)
}

// coolDown is how long a home is left alone after its runs-th silent run.
func coolDown(runs int) time.Duration {
	if runs < SilentRuns {
		return 0
	}
	d := silentCoolDown
	for i := SilentRuns; i < runs && d < silentCoolDownCap; i++ {
		d *= 2
	}
	return min(d, silentCoolDownCap)
}

// Silences is every home's record, by harness and label.
type Silences map[Ref]Silence

// Default is the record of a harness's own default login when it keeps the
// harness not ready at now.
func (s Silences) Default(harness string, now time.Time) (Silence, bool) {
	x, ok := s[Ref{Harness: harness}]
	return x, ok && x.Holds(now)
}

// LoadSilences reads every home's record. A nil q is a runner with no state
// database yet, which has none.
func LoadSilences(ctx context.Context, q *db.Queries) (Silences, error) {
	if q == nil {
		return nil, nil
	}
	rows, err := q.ListHarnessSilence(ctx)
	if err != nil {
		return nil, err
	}
	out := make(Silences, len(rows))
	for _, r := range rows {
		s := Silence{Harness: r.Harness, Label: r.Label, Runs: int(r.Runs), At: time.UnixMilli(r.UpdatedAt).UTC()}
		if r.Until > 0 {
			s.Until = time.UnixMilli(r.Until).UTC()
		}
		out[Ref{Harness: r.Harness, Label: r.Label}] = s
	}
	return out, nil
}

// NoteSilent records one more run on a home that ended harness_silent and
// returns the home's record after it. The count and the cool-down are two
// statements, so the caller runs this in a transaction: two turns ending at
// once must not both read the same count.
//
// The cool-down never shortens: a run started before the home tripped and
// ending after it is one more silent run, and starts the longer one.
func NoteSilent(ctx context.Context, q *db.Queries, harness, label string, now time.Time) (Silence, error) {
	runs, err := q.NoteHarnessSilent(ctx, db.NoteHarnessSilentParams{Harness: harness, Label: label, UpdatedAt: now.UnixMilli()})
	if err != nil {
		return Silence{}, err
	}
	s := Silence{Harness: harness, Label: label, Runs: int(runs)}
	if d := coolDown(s.Runs); d > 0 {
		s.Until = now.Add(d).UTC()
		if err := q.SetHarnessSilenceUntil(ctx, db.SetHarnessSilenceUntilParams{Until: s.Until.UnixMilli(), Harness: harness, Label: label}); err != nil {
			return Silence{}, err
		}
	}
	return s, nil
}

// NoteSpoke records that a turn on a home produced an event: whatever was
// wrong with the home, a harness got a word out in it.
func NoteSpoke(ctx context.Context, q *db.Queries, harness, label string) error {
	return q.ClearHarnessSilence(ctx, db.ClearHarnessSilenceParams{Harness: harness, Label: label})
}

// EndedAtStart is accounts and silences as they read once a daemon that
// started at started has ended every cool-down begun before it: what
// EndCoolDowns writes, for a reader that may come first. The daemon's first
// capability document is built before its store is open, and without this it
// would tell every hub of a cool-down health has already ended.
func EndedAtStart(accounts []Account, silences Silences, started time.Time) ([]Account, Silences) {
	ended := map[Ref]bool{}
	out := make(Silences, len(silences))
	for r, s := range silences {
		if s.At.Before(started) && !s.Until.IsZero() && s.Until.After(started) {
			s.Until = started
			ended[r] = true
		}
		out[r] = s
	}
	if len(ended) == 0 {
		return accounts, silences
	}
	acc := make([]Account, len(accounts))
	for i, a := range accounts {
		if ended[Ref{Harness: a.Harness, Label: a.Label}] {
			a.SilentUntil, a.SilentRuns = nil, 0
		}
		acc[i] = a
	}
	return acc, out
}

// EndCoolDowns offers every home in a cool-down again from now, keeping the
// counts: the daemon's start is the owner's word that the cause is fixed, and
// one more silent run trips the home again at once.
func EndCoolDowns(ctx context.Context, q *db.Queries, now time.Time) error {
	return q.EndHarnessSilenceCoolDowns(ctx, now.UnixMilli())
}
