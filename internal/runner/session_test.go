package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/store/db"
)

func session(t *testing.T, e *env, id string) db.Session {
	t.Helper()
	s, err := e.store.GetSession(context.Background(), db.GetSessionParams{Connection: "hub", ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// continued is a run continuing a session the runner already holds.
func continued(id, session string) v1.Run {
	r := testRun(id, session)
	r.Session.New = false
	return r
}

// runOne takes one queued run through the claim, the executor and the
// reporter, so the hub has its result and will offer the session's next run.
func runOne(t *testing.T, e *env, l *Loop, x *Exec, run v1.Run) {
	t.Helper()
	e.enqueue(t, run)
	claimAndRun(t, l, x)
	e.reporter(l).Flush(context.Background())
	if _, ok := outboxResult(t, e, run.RunID); ok {
		t.Fatalf("run %s: its result is still owed to the hub", run.RunID)
	}
}

// A run naming a session the runner holds resumes it: the adapter is handed
// the native id the first run pinned and the same workdir, with the files the
// first run left in it, and the session's last use moves to the run's end.
func TestASessionResumesInItsWorkdir(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	// The second run lasts a measurable while, from its spawn: a last use
	// written while it prepared or spawned is before spawned+turn, and only
	// one written at its end is at or after it.
	const turn = 50 * time.Millisecond
	var spawned time.Time
	h := &fake.Adapter{ID: "claude", Next: func(spec adapter.Spec) fake.Script {
		spawned = time.Now()
		return fake.Script{Delay: turn, Events: []v1.Event{{Kind: v1.EventText, Text: "hi"}},
			Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"}}
	}}
	x := e.executor(h)

	runOne(t, e, l, x, testRun("a", "s1"))
	first := session(t, e, "s1")
	if err := os.WriteFile(filepath.Join(first.Workdir, "notes.txt"), []byte("left by a"), 0o600); err != nil {
		t.Fatal(err)
	}
	runOne(t, e, l, x, continued("b", "s1"))
	ended := spawned.Add(turn).UnixMilli()

	if len(h.Starts) != 2 {
		t.Fatalf("%d starts, want 2", len(h.Starts))
	}
	if h.Starts[0].NativeSessionID != "" {
		t.Errorf("the first run was handed native id %q; a new session has none", h.Starts[0].NativeSessionID)
	}
	if got := h.Starts[1]; got.NativeSessionID != "native-1" || got.Workdir != first.Workdir {
		t.Errorf("the second run was started in %s resuming %q; want %s resuming native-1", got.Workdir, got.NativeSessionID, first.Workdir)
	}
	if b, err := os.ReadFile(filepath.Join(h.Starts[1].Workdir, "notes.txt")); err != nil || string(b) != "left by a" {
		t.Errorf("the first run's file in the second run's workdir: %q, %v", b, err)
	}
	after := session(t, e, "s1")
	if after.LastUsedAt < ended || after.Workdir != first.Workdir || after.NativeID.String != "native-1" || after.State != "open" {
		t.Errorf("session after the second run = %+v; last use should be the run's end, at or after %d", after, ended)
	}
	if r := hubResult(t, e, "b"); r.State != v1.RunSucceeded {
		t.Errorf("second run = %+v", r)
	}
}

// A workdir that vanished between runs — deleted by hand — is made again at
// the recorded path rather than failing the run: the transcript is the
// conversation, and it is not in the workdir.
func TestAMissingWorkdirIsRecreated(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	h := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"}})
	x := e.executor(h)
	runOne(t, e, l, x, testRun("a", "s1"))
	dir := session(t, e, "s1").Workdir
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	runOne(t, e, l, x, continued("b", "s1"))
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() || h.Starts[1].Workdir != dir {
		t.Errorf("workdir %s after the second run: %v, %v; run started in %s", dir, fi, err, h.Starts[1].Workdir)
	}
}

// The native id is pinned when the harness is spawned, before it says
// anything: a runner that dies in between must still know what to resume.
func TestTheNativeIDIsPinnedAtSpawn(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	x := e.executor(fakeHarness(fake.Script{Hang: true, Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"}}))
	e.enqueue(t, testRun("a", "s1"))
	l.Executor = x
	mustSync(t, l)
	mustSync(t, l)
	deadline := time.Now().Add(10 * time.Second)
	for session(t, e, "s1").NativeID.String != "native-1" {
		if time.Now().After(deadline) {
			t.Fatal("the native id was not recorded while the turn was silent")
		}
		time.Sleep(5 * time.Millisecond)
	}
	x.CancelAll("test over")
	x.Wait()
}

// What a resume the harness turns down looks like to the hub: a transcript
// that is gone is resume_rejected, in the result and in the event before it;
// a harness that ran another conversation is session_mismatch; and neither
// moves the session's resume pointer.
func TestResumeFailuresAreClassed(t *testing.T) {
	for _, tc := range []struct {
		adapterClass, hubClass string
	}{
		{adapter.ClassSessionNotFound, ClassResumeRejected},
		{adapter.ClassSessionMismatch, adapter.ClassSessionMismatch},
		{adapter.ClassHarness, adapter.ClassHarness},
	} {
		t.Run(tc.adapterClass, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			second := false
			x := e.executor(&fake.Adapter{ID: "claude", Next: func(spec adapter.Spec) fake.Script {
				if !second {
					second = true
					return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"}}
				}
				err := &v1.RunError{Class: tc.adapterClass, Message: "the harness said no"}
				return fake.Script{
					Events:  []v1.Event{{Kind: v1.EventError, Error: err}},
					Outcome: adapter.Outcome{State: v1.RunFailed, Error: err, NativeSessionID: "native-1"},
				}
			}})
			runOne(t, e, l, x, testRun("a", "s1"))
			e.enqueue(t, continued("b", "s1"))
			claimAndRun(t, l, x)

			res, ok := outboxResult(t, e, "b")
			if !ok || res.State != v1.RunFailed || res.Error == nil || res.Error.Class != tc.hubClass {
				t.Fatalf("result = %+v (error %+v), %v; want class %s", res, res.Error, ok, tc.hubClass)
			}
			evs, err := e.store.UnackedEvents(context.Background(), db.UnackedEventsParams{Connection: "hub", RunID: "b", Limit: 10})
			if err != nil || len(evs) != 1 {
				t.Fatalf("events = %v, %v", evs, err)
			}
			var ev v1.Event
			if err := json.Unmarshal([]byte(evs[0].Body), &ev); err != nil || ev.Error == nil || ev.Error.Class != tc.hubClass {
				t.Errorf("event = %+v, %v; want class %s", ev, err, tc.hubClass)
			}
			if s := session(t, e, "s1"); s.NativeID.String != "native-1" || s.State != "open" {
				t.Errorf("session after the failed resume = %+v", s)
			}
		})
	}
}

// session_not_found on a run that resumed nothing is not a rejected resume,
// and stays as the adapter named it.
func TestHubClass(t *testing.T) {
	for _, tc := range []struct {
		class   string
		resumed bool
		want    string
	}{
		{adapter.ClassSessionNotFound, true, ClassResumeRejected},
		{adapter.ClassSessionNotFound, false, adapter.ClassSessionNotFound},
		{adapter.ClassSessionMismatch, true, adapter.ClassSessionMismatch},
		{adapter.ClassUsageLimit, true, adapter.ClassUsageLimit},
	} {
		if got := hubClass(tc.class, tc.resumed); got != tc.want {
			t.Errorf("hubClass(%s, %v) = %s, want %s", tc.class, tc.resumed, got, tc.want)
		}
	}
}
