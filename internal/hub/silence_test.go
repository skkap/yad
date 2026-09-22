package hub

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// The lease an offer carries in the fixture's hub: four default intervals.
const fixtureLease = missedIntervals * DefaultSyncInterval

// An offer carries the same lease as a claim (decision 0046). Listed within
// it, the run is claimed. Listed after it, the claim is refused with a cancel
// — the run went back in the queue at the lapse and may be another runner's
// by now — and it is not offered back in the answer carrying that cancel,
// only at a later sync.
func TestAnOfferLapsesWithItsLease(t *testing.T) {
	for _, tc := range []struct {
		name string
		// silent is how long r1 goes without a sync after the offer.
		silent time.Duration
		// r2Takes has another runner sync during the silence.
		r2Takes bool
		// claimed is whether r1's late listing claims the run; offeredBack
		// whether the sync after the cancel offers it to r1 again.
		claimed, offeredBack bool
	}{
		{name: "listed within the lease", silent: fixtureLease - time.Millisecond, claimed: true},
		{name: "listed after the lease", silent: fixtureLease, offeredBack: true},
		{name: "listed after another runner took it", silent: fixtureLease, r2Takes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			c1, c2 := f.register(t, "r1"), f.register(t, "r2")
			f.enqueue(t, run("a", "s1"))
			if res := f.mustSync(t, "r1", c1, first("r1", 1)); !slices.Equal(ids(res.Runs), []string{"a"}) {
				t.Fatalf("offered %v, want [a]", ids(res.Runs))
			}
			f.clock.Advance(tc.silent)
			if tc.r2Takes {
				if res := f.mustSync(t, "r2", c2, first("r2", 1)); !slices.Equal(ids(res.Runs), []string{"a"}) {
					t.Fatalf("r2 offered %v after r1's offer lapsed, want [a]", ids(res.Runs))
				}
			}

			// Free capacity is declared, so the hub could hand the run
			// straight back in the answer that cancels it.
			res := f.mustSync(t, "r1", c1, req("r1", 1, claimed("a")...))
			switch {
			case tc.claimed && (len(cancels(res)) != 0 || f.state(t, "a") != "claimed"):
				t.Fatalf("a claim within the lease: cancels %v, state %s", cancels(res), f.state(t, "a"))
			case !tc.claimed && !slices.Equal(cancels(res), []string{"a"}):
				t.Fatalf("a claim after the lease: cancels %v, want [a]", cancels(res))
			case !tc.claimed && slices.Contains(ids(res.Runs), "a"):
				t.Fatalf("the answer cancelling a also offers it: %v", ids(res.Runs))
			}
			if tc.claimed {
				return
			}
			if tc.r2Takes {
				if res := f.mustSync(t, "r2", c2, req("r2", 0, claimed("a")...)); len(cancels(res)) != 0 || f.state(t, "a") != "claimed" {
					t.Errorf("r2's claim after r1's late one: cancels %v, state %s", cancels(res), f.state(t, "a"))
				}
			}
			res = f.mustSync(t, "r1", c1, req("r1", 1))
			if got := slices.Contains(ids(res.Runs), "a"); got != tc.offeredBack {
				t.Errorf("the sync after the cancel offered %v; offered a again = %v, want %v", ids(res.Runs), got, tc.offeredBack)
			}
		})
	}
}

// A runner silent for longer than abandon-after is given up (decision 0046):
// its sessions close and the runs queued in them fail, naming what to do
// instead. Its credential stays. Back, it syncs as it always did, hears
// close_session for the session it still has a workdir for until it reports
// that close, and takes new work. Silent for less, nothing changes.
func TestASilentRunnersSessionsAreGivenUp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		silent time.Duration
		given  bool
	}{
		{"silent past abandon-after", DefaultAbandonAfter + time.Millisecond, true},
		{"silent exactly abandon-after", DefaultAbandonAfter, true},
		{"silent under abandon-after", DefaultAbandonAfter - time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t)
			cred := f.register(t, "r1")
			f.claimedBy(t, "r1", cred, "a1")      // binds s-a1 to r1
			f.enqueue(t, continues("a2", "s-a1")) // waits behind a1, for r1 alone

			f.clock.Advance(tc.silent)
			if err := f.hub.Sweep(ctx); err != nil {
				t.Fatal(err)
			}
			if got := f.state(t, "a1"); got != "lost" {
				t.Errorf("a1, whose lease lapsed a day ago: %s, want lost", got)
			}
			sess, err := f.store.GetSession(ctx, "s-a1")
			if err != nil {
				t.Fatal(err)
			}
			a2, err := f.store.GetRun(ctx, "a2")
			if err != nil {
				t.Fatal(err)
			}
			if !tc.given {
				if sess.ClosedAt.Valid || a2.State != "queued" {
					t.Fatalf("given up early: session closed %v, a2 %s", sess.ClosedAt.Valid, a2.State)
				}
				if res := f.mustSync(t, "r1", cred, req("r1", 1)); !slices.Equal(ids(res.Runs), []string{"a2"}) {
					t.Errorf("r1 back before abandon-after is offered %v, want its session's a2", ids(res.Runs))
				}
				return
			}
			if !sess.ClosedAt.Valid || sess.CloseReason.String != string(v1.SessionClosed) || sess.RunnerID.String != "r1" {
				t.Errorf("session s-a1: closed %v (%q), bound to %q; want closed, still r1's", sess.ClosedAt.Valid, sess.CloseReason.String, sess.RunnerID.String)
			}
			if a2.State != "failed" || !strings.Contains(a2.Reason.String, "submit the work to a new session") || !strings.Contains(a2.Reason.String, "r1") {
				t.Errorf("a2: %s %q, want failed naming the runner and what to do", a2.State, a2.Reason.String)
			}

			// The credential still works, and the runner hears about the
			// session it holds a workdir for.
			f.enqueue(t, run("b1", "s-b1"))
			res := f.mustSync(t, "r1", cred, req("r1", 1))
			if !slices.ContainsFunc(res.Controls, func(c v1.Control) bool {
				return c.Kind == v1.ControlCloseSession && c.SessionID == "s-a1"
			}) {
				t.Errorf("the returning runner's answer carries %+v, and no close_session for s-a1", res.Controls)
			}
			if !slices.Equal(ids(res.Runs), []string{"b1"}) {
				t.Errorf("the returning runner is offered %v, want new work", ids(res.Runs))
			}
			// The runner's report answers the close, whatever reason it
			// gives, and the control stops.
			back := req("r1", 0, claimed("b1")...)
			back.ClosedSessions = []v1.ClosedSession{{SessionID: "s-a1", Reason: v1.SessionClosed, ClosedAt: f.clock.Now()}}
			f.mustSync(t, "r1", cred, back)
			res = f.mustSync(t, "r1", cred, req("r1", 0, claimed("b1")...))
			for _, c := range res.Controls {
				if c.Kind == v1.ControlCloseSession {
					t.Errorf("close_session for %s repeated after the runner reported it", c.SessionID)
				}
			}
			if sess, _ := f.store.GetSession(ctx, "s-a1"); sess.CloseReason.String != string(v1.SessionClosed) {
				t.Errorf("the report changed the reason the hub closed with to %q", sess.CloseReason.String)
			}
		})
	}
}

// Silence counts from the runner's last sync: a runner that keeps syncing is
// never given up, however long its session lives.
func TestSyncingKeepsARunnerFromBeingGivenUp(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.claimedBy(t, "r1", cred, "a1")
	f.enqueue(t, continues("a2", "s-a1"))
	for range 3 {
		f.clock.Advance(DefaultAbandonAfter / 2)
		f.mustSync(t, "r1", cred, req("r1", 0, v1.HeldRun{RunID: "a1", State: v1.RunRunning}))
		if err := f.hub.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if sess, _ := f.store.GetSession(ctx, "s-a1"); sess.ClosedAt.Valid {
		t.Errorf("a runner that syncs every twelve hours was given up")
	}
	if got := f.state(t, "a2"); got != "queued" {
		t.Errorf("a2 = %s, want queued", got)
	}
}

// A hub that was down heard from nobody: silence starts no earlier than the
// hub's own start, so a restart after a long outage gives every runner a full
// abandon-after to come back.
func TestAHubCountsSilenceFromItsOwnStart(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.claimedBy(t, "r1", cred, "a1")
	f.enqueue(t, continues("a2", "s-a1"))

	f.clock.Advance(2 * DefaultAbandonAfter)
	restarted := New(Options{Store: f.store, Now: f.clock.Now})
	if err := restarted.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.state(t, "a2"); got != "queued" {
		t.Fatalf("a2 = %s on the first sweep after a restart, want queued", got)
	}
	f.clock.Advance(DefaultAbandonAfter)
	if err := restarted.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.state(t, "a2"); got != "failed" {
		t.Errorf("a2 = %s a full abandon-after after the restart, want failed", got)
	}
}

// abandon-after must outlast the lease: shorter, the hub would give up a
// runner whose runs are still leased to it.
func TestValidateAbandonAfter(t *testing.T) {
	_, lease := timings(DefaultSyncInterval)
	for _, tc := range []struct {
		d  time.Duration
		ok bool
	}{
		{DefaultAbandonAfter, true},
		{lease + time.Millisecond, true},
		{lease, false},
		{30 * time.Second, false},
		{0, false},
		{-time.Hour, false},
	} {
		err := ValidateAbandonAfter(tc.d, DefaultSyncInterval)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v, want ok=%v", tc.d, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "name more than") {
			t.Errorf("%s: %q names no next action", tc.d, err)
		}
	}
}
