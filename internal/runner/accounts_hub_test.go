package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// Adding and removing accounts from a hub (decision 0057).

func addLogin(c v1.Control) v1.Control {
	c.Add = true
	return c
}

// listedInConfig is what config.toml on disk lists for claude.
func listedInConfig(t *testing.T, p config.Paths) []string {
	t.Helper()
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c.Harness["claude"].Accounts
}

// An add is a login allowed to create its label: listed in config.toml and in
// the lists every hub's health reads only once the login took, and free, so
// runs take it without a restart. Until then nothing lists it.
func TestAHubAddsAnAccount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start v1.Control
	}{
		{"by link", addLogin(startLogin("lg1", "claude", "second"))},
		{"by token", addLogin(v1.Control{Kind: v1.ControlLoginToken, LoginID: "lg1", Harness: "claude", Account: "second", Token: "sk-ant-oat01-fake-token"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			r := newLoginRig(t, e, accountConfig("work"), "")
			r.Control("hub", tc.start)
			second := account.Ref{Harness: "claude", Label: "second"}
			if tc.start.Kind == v1.ControlStartLogin {
				r.until(t, "lg1", v1.LoginWaiting)
				if r.Accounts.Lists().Has(second) || slices.Contains(listedInConfig(t, e.paths), "second") {
					t.Fatal("the account was listed before its login took")
				}
				r.Control("hub", loginCode("lg1", fakeLoginCode))
			}
			rep := r.until(t, "lg1", v1.LoginSucceeded)
			if rep.Account != "second" {
				t.Errorf("the report is %+v", rep)
			}
			if got := listedInConfig(t, e.paths); !slices.Equal(got, []string{"work", "second"}) {
				t.Errorf("config.toml lists %v, want the new account after the owner's", got)
			}
			if !r.Accounts.Lists().Has(second) {
				t.Error("the running lists do not name the new account, so no run takes it until a restart")
			}
			if got := accountState(t, e, "second"); got != v1.AccountFree {
				t.Errorf("the added account is %q, want free", got)
			}
			if r.changed.Load() == 0 {
				t.Error("nothing was told, so the capability document waits for its next probe")
			}
		})
	}
}

// An add that does not take lists nothing, and keeps the home it made for a
// retry with the same label, as `yad account add` does (decision 0043).
func TestAnAddThatDoesNotTakeListsNothing(t *testing.T) {
	e := newEnv(t)
	r := newLoginRig(t, e, accountConfig("work"), "")
	r.Control("hub", addLogin(startLogin("lg1", "claude", "second")))
	r.until(t, "lg1", v1.LoginWaiting)
	r.Control("hub", loginCode("lg1", "not-the-code"))
	r.until(t, "lg1", v1.LoginFailed)
	if got := listedInConfig(t, e.paths); !slices.Equal(got, []string{"work"}) {
		t.Errorf("config.toml lists %v after a login that did not take", got)
	}
	if r.Accounts.Lists().Has(account.Ref{Harness: "claude", Label: "second"}) {
		t.Error("the lists name an account whose login did not take")
	}
	if _, err := os.Stat(account.HomeDir(e.paths.Data, "claude", "second")); err != nil {
		t.Errorf("the home was not kept for the next try: %v", err)
	}

	// The retry, with the same label, takes and lists it.
	r.Control("hub", addLogin(startLogin("lg2", "claude", "second")))
	r.until(t, "lg2", v1.LoginWaiting)
	r.Control("hub", loginCode("lg2", fakeLoginCode))
	r.until(t, "lg2", v1.LoginSucceeded)
	if got := listedInConfig(t, e.paths); !slices.Equal(got, []string{"work", "second"}) {
		t.Errorf("config.toml lists %v after the retry took", got)
	}
}

// Every add the runner will not do ends failed before anything is touched,
// with a next action — a command where there is one, built to run as
// printed and naming no path.
func TestAnAddTheRunnerRefusesSaysWhatToDo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		c      v1.Control
		manage bool
		says   string
		want   []string
	}{
		{"a label already listed", addLogin(startLogin("lg1", "claude", "work")), true, "start the login without add", nil},
		{"a label yad does not take", addLogin(startLogin("lg1", "claude", "Not A Label")), true, "not an account label", nil},
		{"no label", addLogin(startLogin("lg1", "claude", "")), true, "names none", nil},
		{"a token with no label", addLogin(v1.Control{Kind: v1.ControlLoginToken, LoginID: "lg1", Harness: "claude", Token: "sk-ant-oat01-x"}), true, "names none", nil},
		{"a hub the owner has not let", addLogin(startLogin("lg1", "claude", "second")), false, "manage_accounts = false",
			[]string{"yad", "--profile", "test", "account", "add", "claude", "second"}},
		{"a hub the owner has not let, and a bad label", addLogin(startLogin("lg1", "claude", "x`y")), false, "manage_accounts = false",
			[]string{"yad", "--profile", "test", "account", "add", "claude", "<label>"}},
		// Without add, an unlisted label says how to add it, by the rule of
		// the hub asking.
		{"no add, from a hub that may", startLogin("lg1", "claude", "second"), true, "start the login again with add",
			[]string{"yad", "--profile", "test", "account", "add", "claude", "second"}},
		{"no add, from a hub that may not", startLogin("lg1", "claude", "second"), false, "has not let this hub add",
			[]string{"yad", "--profile", "test", "account", "add", "claude", "second"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			r := newLoginRig(t, e, accountConfig("work"), "")
			r.MayManage = func(string) bool { return tc.manage }
			r.Control("hub", tc.c)
			rep := r.until(t, "lg1", v1.LoginFailed)
			if !strings.Contains(rep.Error, tc.says) {
				t.Errorf("the error %q does not say %q", rep.Error, tc.says)
			}
			if strings.Contains(rep.Error, e.paths.Data) || strings.Contains(rep.Error, e.paths.Config) {
				t.Errorf("the error names a path: %q", rep.Error)
			}
			if tc.want != nil {
				cmds := shellwordtest.Commands(rep.Error, "yad ")
				if len(cmds) != 1 {
					t.Fatalf("want one command in %q", rep.Error)
				}
				shellwordtest.Check(t, cmds[0], tc.want...)
			}
			if got := listedInConfig(t, e.paths); !slices.Equal(got, []string{"work"}) {
				t.Errorf("a refused add changed config.toml: %v", got)
			}
			if _, err := os.Stat(account.HomeDir(e.paths.Data, "claude", "second")); !os.IsNotExist(err) {
				t.Errorf("a refused add made a home (%v)", err)
			}
		})
	}
}

// remove_account is `yad account remove` through the daemon: listed nowhere
// at once, a run on it finishes there and its home goes when that run lets
// go, a login in flight on it ends cancelled, and a repeat, or a label never
// listed, does nothing.
func TestAHubRemovesAnAccount(t *testing.T) {
	e := newEnv(t)
	r := newLoginRig(t, e, accountConfig("work", "spare"), "")
	ctx := context.Background()
	work, spare := account.Ref{Harness: "claude", Label: "work"}, account.Ref{Harness: "claude", Label: "spare"}
	workHome := plantCredential(t, e.paths.Data, "work")
	spareHome := plantCredential(t, e.paths.Data, "spare")

	// A run on work, and a login in flight on spare.
	hold, ok := r.Accounts.take(work, "run-1")
	if !ok {
		t.Fatal("the run could not take the account")
	}
	r.Control("hub", startLogin("lg1", "claude", "spare"))
	r.until(t, "lg1", v1.LoginWaiting)

	res, removed, err := r.Accounts.Remove(ctx, e.paths, work)
	if err != nil || !removed {
		t.Fatalf("remove: %v, %v", removed, err)
	}
	if !slices.Equal(res.Runs, []string{"run-1"}) {
		t.Errorf("the runs still on it are %v", res.Runs)
	}
	if got := listedInConfig(t, e.paths); !slices.Equal(got, []string{"spare"}) {
		t.Errorf("config.toml lists %v", got)
	}
	if r.Accounts.Lists().Has(work) {
		t.Error("the lists still name the removed account")
	}
	if _, ok := r.Accounts.take(work, "run-2"); ok {
		t.Error("a new run took the removed account")
	}
	if _, err := os.Stat(workHome); err != nil {
		t.Fatalf("the home went while a run was on it: %v", err)
	}
	r.Accounts.release(hold)
	if _, err := os.Stat(workHome); !os.IsNotExist(err) {
		t.Errorf("the home is still there after the last run let go (%v)", err)
	}

	if _, _, err := r.Accounts.Remove(ctx, e.paths, spare); err != nil {
		t.Fatal(err)
	}
	rep := r.until(t, "lg1", v1.LoginCancelled)
	if !strings.Contains(rep.Error, "was removed") {
		t.Errorf("the login ended %q, not saying the account was removed", rep.Error)
	}
	r.Close()
	if _, err := os.Stat(spareHome); !os.IsNotExist(err) {
		t.Errorf("the home is still there once the login let go (%v)", err)
	}

	// A repeat finds nothing, and a label nothing lists is not a removal.
	for _, again := range []account.Ref{work, {Harness: "claude", Label: "stranger"}} {
		if _, removed, err := r.Accounts.Remove(ctx, e.paths, again); err != nil || removed {
			t.Errorf("removing %s again: removed %v, %v", again.Label, removed, err)
		}
	}
	if got := listedInConfig(t, e.paths); len(got) != 0 {
		t.Errorf("config.toml lists %v at the end", got)
	}
}

// The accounts feature goes to a hub whose owner lets it add and remove, and
// only beside login; so the fingerprint is each hub's own. remove_account
// from a hub its owner has not let is ignored.
func TestTheAccountsFeatureIsPerConnection(t *testing.T) {
	e := newEnv(t)
	base := drivableDoc("r1", 1)
	base.ProtocolFeatures = capability.Features(base.Harnesses)
	noLogin := base
	noLogin.ProtocolFeatures = slices.DeleteFunc(slices.Clone(base.ProtocolFeatures), func(f string) bool { return f == capability.FeatureLogin })
	for _, tc := range []struct {
		name   string
		doc    v1.Capabilities
		manage bool
		want   bool
	}{
		{"allowed", base, true, true},
		{"manage_accounts = false", base, false, false},
		{"allowed, without login", noLogin, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &Loop{Capabilities: func() v1.Capabilities { return tc.doc }, ManageAccounts: tc.manage}
			got := l.document()
			if slices.Contains(got.ProtocolFeatures, capability.FeatureAccounts) != tc.want {
				t.Errorf("features %v, want accounts: %v", got.ProtocolFeatures, tc.want)
			}
			if slices.Contains(tc.doc.ProtocolFeatures, capability.FeatureAccounts) {
				t.Error("the runner's own document was changed, which every other hub is sent")
			}
			if tc.want && capability.Fingerprint(got) == capability.Fingerprint(tc.doc) {
				t.Error("the fingerprint does not move with the feature, so a hub never asks for the document that says it")
			}
		})
	}

	t.Run("remove_account from a hub that may not", func(t *testing.T) {
		if err := config.Save(e.paths, accountConfig("work")); err != nil {
			t.Fatal(err)
		}
		l := &Loop{Connection: "hub", Accounts: accountsOf(e.paths.Data, accountConfig("work")), Paths: e.paths}
		l.init()
		l.removeAccount(context.Background(), v1.Control{Kind: v1.ControlRemoveAccount, Harness: "claude", Account: "work"})
		if got := listedInConfig(t, e.paths); !slices.Equal(got, []string{"work"}) {
			t.Errorf("config.toml lists %v", got)
		}
		if _, err := os.Stat(filepath.Join(e.paths.Config, "config.toml")); err != nil {
			t.Fatal(err)
		}
	})
}

// Changes to the lists take turns, and each reloads config.toml as it then
// reads. An add whose reload is held up — here by a store not yet open, which
// every reload waits for — does not let a removal written after it be undone
// when the add's reload finally lands: the removal waits for the add, not the
// other way round. Without the turns the two reload in either order, and the
// removed account comes back in the daemon while config.toml says it is gone.
func TestAnAddReloadingLateDoesNotUndoARemoval(t *testing.T) {
	x := &Exec{}
	fakeClaudeBinary(t, x)
	for range 6 {
		e := newEnv(t)
		cfg := accountConfig("work", "old")
		if err := config.Save(e.paths, cfg); err != nil {
			t.Fatal(err)
		}
		a := accountsOf(e.paths.Data, cfg)
		a.Binary = x.Binary
		ctx := context.Background()
		plantCredential(t, e.paths.Data, "new")
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, err := a.Add(ctx, e.paths, account.Ref{Harness: "claude", Label: "new"}); err != nil {
				t.Error(err)
			}
		})
		// The add has written and is waiting to reload.
		deadline := time.Now().Add(10 * time.Second)
		for !slices.Contains(listedInConfig(t, e.paths), "new") {
			if time.Now().After(deadline) {
				t.Fatal("the add never wrote config.toml")
			}
			time.Sleep(5 * time.Millisecond)
		}
		wg.Go(func() {
			if _, _, err := a.Remove(ctx, e.paths, account.Ref{Harness: "claude", Label: "old"}); err != nil {
				t.Error(err)
			}
		})
		time.Sleep(100 * time.Millisecond)
		a.attach(ctx, e.store)
		wg.Wait()
		if got := a.Lists()["claude"]; !slices.Equal(got, []string{"work", "new"}) {
			t.Fatalf("the daemon lists %v, config.toml %v", got, listedInConfig(t, e.paths))
		}
	}
}
