package runner

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
)

// loginRemoval is a daemon holding the rig's logins, mid self-update, whose
// owner is about to remove the connection "hub": nothing else is wired, since
// a removal with no loop of its own goes straight to ending what it left.
func (r *loginRig) loginRemoval() *server {
	d := NewDrain()
	d.Update("a release is pending")
	return &server{drain: d, store: r.e.store, exec: &Exec{}, logins: r.Logins, monitor: NewMonitor(),
		sessions:   &Collector{Store: r.e.store, Workdirs: filepath.Join(r.e.paths.Data, "workdirs"), Log: slog.New(slog.DiscardHandler)},
		configured: map[string]bool{"hub": true, "other": true}, removed: map[string]bool{}, retired: map[string]bool{},
		log: slog.New(slog.DiscardHandler)}
}

// waitFor waits until ok, or for a bound no working build comes near; the
// caller says what it found either way.
func (r *loginRig) waitFor(ok func() bool) {
	deadline := time.Now().Add(rigLoginWait)
	for time.Now().Before(deadline) && !ok() {
		time.Sleep(10 * time.Millisecond)
	}
}

// A hub login is its hub's alone: once `yad disconnect` has retired the
// runner there, no login_code or cancel_login can reach it, and no sync will
// carry its end (DEV-182). The removal ends it as a cancel would, and forgets
// it once it has let go of the home. Left alone, a link login kept the
// harness's process in the account's home until its code wait ran out, kept
// the runner from idling — a self-update waited on it — and its end stayed
// owed for the life of the process, which every update's drain then waited
// flushWait on, holding one of maxLogins all the while.
func TestRemovingAConnectionEndsItsHubLogins(t *testing.T) {
	cancel := v1.Control{Kind: v1.ControlCancelLogin, LoginID: "lg1"}
	for _, tc := range []struct {
		name string
		// before is where the hub left its login when the owner removed it.
		before func(t *testing.T, r *loginRig)
	}{
		{"waiting on its code", func(*testing.T, *loginRig) {}},
		{"cancelled, still letting go of the home", func(_ *testing.T, r *loginRig) {
			r.Control("hub", cancel)
		}},
		{"ended, its end not yet carried", func(t *testing.T, r *loginRig) {
			r.Control("hub", cancel)
			r.until(t, "lg1", v1.LoginCancelled)
			r.waitFor(func() bool { return !r.Busy() })
			// And one this runner never had, answered all the same.
			r.Control("hub", v1.Control{Kind: v1.ControlCancelLogin, LoginID: "never"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			r := newLoginRig(t, e, accountConfig("work"), "")
			// A person who walked away: longer than the test, as the shipped
			// ten minutes are.
			r.CodeWait = time.Hour
			r.Control("hub", startLogin("lg1", "claude", "work"))
			r.until(t, "lg1", v1.LoginWaiting)
			tc.before(t, r)

			sv := r.loginRemoval()
			if _, err := sv.remove(context.Background(), "hub"); err != nil {
				t.Fatal(err)
			}
			r.waitFor(func() bool { return !r.Busy() && !r.Owed() })
			if r.Busy() {
				t.Errorf("the removed hub's login is still in flight in the account's home: %+v", r.Reports("hub"))
			}
			if r.Owed() {
				t.Errorf("the removed hub is still owed its login's end, which no sync will ever carry: %+v", r.Reports("hub"))
			}
			var deadline time.Time
			if sv.loginsHeld(&deadline) {
				t.Error("a self-update's drain still waits on the removed hub's logins")
			}
			if credentialIn(account.HomeDir(e.paths.Data, "claude", "work")) {
				t.Error("the removed hub's login logged the account in")
			}
		})
	}
}

// Another hub's login carries on through the removal: still waiting, still
// reported to its own hub, and taking its code once the owner pastes it.
func TestRemovingAConnectionLeavesOtherHubsLogins(t *testing.T) {
	e := newEnv(t)
	r := newLoginRig(t, e, accountConfig("work", "home"), "")
	r.CodeWait = time.Hour
	r.Control("hub", startLogin("lg1", "claude", "work"))
	r.Control("other", startLogin("lg2", "claude", "home"))
	r.until(t, "lg1", v1.LoginWaiting)
	r.waitFor(func() bool {
		reps := r.Reports("other")
		return len(reps) == 1 && reps[0].State == v1.LoginWaiting
	})

	if _, err := r.loginRemoval().remove(context.Background(), "hub"); err != nil {
		t.Fatal(err)
	}
	r.waitFor(func() bool { return len(r.Reports("hub")) == 0 })
	if reps := r.Reports("hub"); len(reps) > 0 {
		t.Fatalf("the removed hub's logins are still held: %+v", reps)
	}
	if !strings.Contains(r.log.String(), "was removed from this runner") {
		t.Errorf("the log does not say why the removed hub's login ended:\n%s", r.log.String())
	}

	reps := r.Reports("other")
	if len(reps) != 1 || reps[0].LoginID != "lg2" || reps[0].State != v1.LoginWaiting {
		t.Fatalf("the other hub's login is %+v after the removal, not still waiting", reps)
	}
	r.Control("other", loginCode("lg2", fakeLoginCode))
	r.waitFor(func() bool {
		reps := r.Reports("other")
		return len(reps) == 1 && reps[0].State == v1.LoginSucceeded
	})
	if reps := r.Reports("other"); len(reps) != 1 || reps[0].State != v1.LoginSucceeded {
		t.Fatalf("the other hub's login is %+v, not succeeded", reps)
	}
	if !credentialIn(account.HomeDir(e.paths.Data, "claude", "home")) {
		t.Error("the other hub's login did not log its account in")
	}
}
