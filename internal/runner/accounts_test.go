package runner

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
)

func claudeLists(labels ...string) account.Lists {
	return account.ListsOf(accountConfig(labels...))
}

func accountRows(t *testing.T, e *env) []string {
	t.Helper()
	ctx := context.Background()
	rows, err := e.store.ListAllAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	windows, err := e.store.ListAllAccountWindows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, "state:"+r.Label)
	}
	for _, w := range windows {
		out = append(out, "window:"+w.Label)
	}
	slices.Sort(out)
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A daemon starting with rows for accounts config.toml no longer lists prunes
// them: `yad account remove` with no daemon running changes the file and the
// home and writes nothing else (decision 0043), so this is where those rows go.
func TestAStartingRunnerPrunesAccountsNoLongerListed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()
	reset := now.Add(time.Hour)
	for _, label := range []string{"work", "gone"} {
		if err := account.SetLimit(ctx, e.store.Queries, "claude", label, reset, now); err != nil {
			t.Fatal(err)
		}
		if err := account.SetWindows(ctx, e.store.Queries, "claude", label, []v1.AccountWindow{{Name: "five_hour", UsedPercent: 100, ResetsAt: &reset}}, now); err != nil {
			t.Fatal(err)
		}
	}
	m, d := NewMonitor(), NewDrain()
	done := make(chan error, 1)
	cfg := accountConfig("work")
	cfg.Sessions.DiskFloor = 0
	go func() {
		done <- Serve(ctx, Options{Paths: e.paths, Config: cfg, RunnerID: "r",
			Capabilities: func() v1.Capabilities { return drivableDoc("r", 1) },
			Log:          slog.New(slog.DiscardHandler), Monitor: m, Drain: d})
	}()
	eventually(t, "the runner is ready", m.Ready)
	if got, want := accountRows(t, e), []string{"state:work", "window:work"}; !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v — the unlisted account's state and windows go, the listed one's stay", got, want)
	}
	d.Stop("test")
	if err := <-done; err != nil {
		t.Errorf("Serve: %v", err)
	}
}

// The lists a running daemon reads are the ones Reload gave it: an added
// account is read from the next Load on, and a removed one is not.
func TestReloadChangesWhatEveryReaderSees(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	plantCredential(t, e.paths.Data, "spare")
	a := accountsOf(e.paths.Data, accountConfig("work"))
	a.attach(ctx, e.store)
	x := &Exec{}
	fakeClaudeBinary(t, x)
	a.Binary = x.Binary

	labels := func() []string {
		t.Helper()
		got, err := a.Load(ctx, e.store.Queries, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, acct := range got {
			out = append(out, acct.Label)
		}
		return out
	}
	if got := labels(); !slices.Equal(got, []string{"work"}) {
		t.Fatalf("before = %v", got)
	}
	res, err := a.Reload(ctx, claudeLists("work", "spare"), account.Ref{Harness: "claude", Label: "spare"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != v1.AccountFree {
		t.Errorf("the added account reads %q; its home is logged in", res.State)
	}
	// Written, so the login check's answer is the daemon's record and not
	// only the missing-row default.
	if got := accountState(t, e, "spare"); got != v1.AccountFree {
		t.Errorf("the added account's row is %q, want free", got)
	}
	if got := labels(); !slices.Equal(got, []string{"work", "spare"}) {
		t.Errorf("after the add = %v", got)
	}
	if _, err := a.Reload(ctx, claudeLists("spare"), account.Ref{Harness: "claude", Label: "work"}, true); err != nil {
		t.Fatal(err)
	}
	if got := labels(); !slices.Equal(got, []string{"spare"}) {
		t.Errorf("after the remove = %v", got)
	}
	if exists(account.HomeDir(e.paths.Data, "claude", "work")) {
		t.Error("the removed account's home is still there with no run on it")
	}
}

// An account whose login is gone is added back by logging it in again, which
// leaves config.toml as it was: the daemon still asks, and the account comes
// back into service. And the answer is the harness's — a home with no login
// is recorded needs_login however the CLI got there.
func TestReloadAsksAboutTheAccountItIsTold(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	if _, err := account.Ensure(e.paths.Data, "claude", "empty"); err != nil {
		t.Fatal(err)
	}
	if err := account.SetState(ctx, e.store.Queries, "claude", "work", v1.AccountNeedsLogin, time.Now()); err != nil {
		t.Fatal(err)
	}
	a := accountsOf(e.paths.Data, accountConfig("work", "empty"))
	a.attach(ctx, e.store)
	x := &Exec{}
	fakeClaudeBinary(t, x)
	a.Binary = x.Binary
	for _, tc := range []struct {
		label string
		want  v1.AccountState
	}{
		{"work", v1.AccountFree},
		{"empty", v1.AccountNeedsLogin},
	} {
		res, err := a.Reload(ctx, claudeLists("work", "empty"), account.Ref{Harness: "claude", Label: tc.label}, false)
		if err != nil {
			t.Fatal(err)
		}
		if res.State != tc.want || accountState(t, e, tc.label) != tc.want {
			t.Errorf("%s: reads %q, row %q, want %q", tc.label, res.State, accountState(t, e, tc.label), tc.want)
		}
	}
}

// A run on an account the owner removes finishes on it. The home stays until
// the last run lets go, no new run can take the account meanwhile, and what
// that run wrote about the account after the removal is forgotten with it.
func TestARemovedAccountKeepsItsHomeUntilItsLastRunLetsGo(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	home := plantCredential(t, e.paths.Data, "work")
	a := accountsOf(e.paths.Data, accountConfig("work"))
	a.attach(ctx, e.store)
	ref := account.Ref{Harness: "claude", Label: "work"}

	first, ok := a.take(ref, "run-1")
	if !ok {
		t.Fatal("a listed account could not be taken")
	}
	second, _ := a.take(ref, "run-2")
	res, err := a.Reload(ctx, claudeLists(), ref, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Runs, []string{"run-1", "run-2"}) {
		t.Errorf("runs still on it = %v", res.Runs)
	}
	if !exists(home) {
		t.Fatal("the home went while two runs were on it")
	}
	if _, ok := a.take(ref, "run-3"); ok {
		t.Error("a new run took an account the owner removed")
	}
	// The run's turn ends and records what it heard, as runs do.
	if err := account.SetWindows(ctx, e.store.Queries, "claude", "work", []v1.AccountWindow{{Name: "five_hour", UsedPercent: 12}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	a.release(first)
	if !exists(home) {
		t.Fatal("the home went while a run was still on it")
	}
	a.release(second)
	if exists(home) {
		t.Error("the home outlived the last run on the removed account")
	}
	if got := accountRows(t, e); len(got) != 0 {
		t.Errorf("rows left for the removed account: %v", got)
	}
}

// Logged in again under the same label before the old run ended: the home the
// owner just logged in is not the one waiting to be deleted any more.
func TestAnAccountAddedBackBeforeItsRunEndsKeepsItsHome(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	home := plantCredential(t, e.paths.Data, "work")
	a := accountsOf(e.paths.Data, accountConfig("work"))
	a.attach(ctx, e.store)
	ref := account.Ref{Harness: "claude", Label: "work"}
	h, _ := a.take(ref, "run-1")
	if _, err := a.Reload(ctx, claudeLists(), ref, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reload(ctx, claudeLists("work"), ref, false); err != nil {
		t.Fatal(err)
	}
	a.release(h)
	if !exists(home) {
		t.Error("the home of an account added back was deleted when the old run ended")
	}
}

// The daemon acts on what config.toml says, and the CLI wrote the change there
// before asking: a file that says otherwise changed again since, and the
// change is refused rather than half made.
func TestReloadRefusesAChangeTheFileDoesNotShow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	home := plantCredential(t, e.paths.Data, "work")
	a := accountsOf(e.paths.Data, accountConfig("work"))
	ref := account.Ref{Harness: "claude", Label: "work"}
	if _, err := a.Reload(ctx, claudeLists("work"), ref, true); err == nil {
		t.Error("a removal of an account the file still lists was taken")
	}
	if !exists(home) {
		t.Error("a refused removal deleted the home")
	}
	if _, err := a.Reload(ctx, claudeLists(), account.Ref{Harness: "claude", Label: "spare"}, false); err == nil {
		t.Error("an add of an account the file does not list was taken")
	}
	if got := a.Lists(); !slices.Equal(got["claude"], []string{"work"}) {
		t.Errorf("a refused change swapped the lists: %v", got)
	}
}

// The executor takes its account through the same source, so a removal lands
// between two runs: the next pick does not choose it.
func TestTheExecutorPicksFromTheReloadedLists(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	plantCredential(t, e.paths.Data, "spare")
	x, _ := e.accountExecutor(t, accountConfig("work"))
	x.init()
	got, h, ok, err := x.pickAccount(ctx, "claude", "run-1")
	if err != nil || !ok || got.Label != "work" {
		t.Fatalf("picked %q (%v, %v)", got.Label, ok, err)
	}
	x.Accounts.release(h)
	lists := account.ListsOf(config.Config{Harness: map[string]config.HarnessConfig{"claude": {Accounts: []string{"spare"}}}})
	if _, err := x.Accounts.Reload(ctx, lists, account.Ref{Harness: "claude", Label: "work"}, true); err != nil {
		t.Fatal(err)
	}
	got, h, ok, err = x.pickAccount(ctx, "claude", "run-2")
	if err != nil || !ok || got.Label != "spare" {
		t.Fatalf("after the remove, picked %q (%v, %v)", got.Label, ok, err)
	}
	x.Accounts.release(h)
}
