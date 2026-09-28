package runner

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/codex/codextest"
	"github.com/skkap/yad/internal/adapter/fake"

	"context"
)

// The test binary doubles as claude for its login check: re-executed with
// RUNNER_TEST_CLAUDE set, `auth status` answers in JSON the way claude
// does, from the home it was pointed at. Nothing else about claude is faked
// here — the turn itself is the in-memory adapter. Started under the name
// codex, it is codextest's codex, for a hub's device-code login.
func TestMain(m *testing.M) {
	if codextest.Child() {
		codextest.Main()
		os.Exit(0)
	}
	if os.Getenv("RUNNER_TEST_CLAUDE") != "" {
		os.Exit(fakeClaudeAuth(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeClaudeAuth(args []string) int {
	if strings.Join(args, " ") == "auth login" {
		return fakeClaudeLogin()
	}
	if strings.Join(args, " ") != "auth status" {
		fmt.Fprintf(os.Stderr, "unexpected argv %q\n", args)
		return 2
	}
	// A claude whose login check cannot answer: an older build without the
	// subcommand, or one whose output shape moved.
	if os.Getenv("RUNNER_TEST_CLAUDE") == "broken" {
		fmt.Fprintln(os.Stderr, "unknown command: auth")
		return 1
	}
	home := os.Getenv("CLAUDE_CONFIG_DIR")
	// A claude whose check answers once in this home and then cannot: the
	// second of two checks timing out.
	if os.Getenv("RUNNER_TEST_CLAUDE") == "once" {
		asked := filepath.Join(home, ".yad-test-status-asked")
		if _, err := os.Stat(asked); err == nil {
			fmt.Fprintln(os.Stderr, "timed out")
			return 1
		}
		if err := os.WriteFile(asked, nil, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	_, err := os.Stat(filepath.Join(home, ".credentials.json"))
	// As claude does: any CLAUDE_CODE_OAUTH_TOKEN is a login to its check.
	in := err == nil || os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != ""
	b, _ := json.Marshal(map[string]any{"loggedIn": in, "email": "owner@example.com"})
	fmt.Println(string(b))
	return 0
}

// fakeClaudeBinary points the executor's login check at this test binary.
// mode "broken" is a harness whose login check cannot answer at all, and
// "once" one that answers only the first time it is asked in a home.
func fakeClaudeBinary(t *testing.T, x *Exec, mode ...string) {
	t.Helper()
	m := "1"
	if len(mode) > 0 {
		m = mode[0]
	}
	t.Setenv("RUNNER_TEST_CLAUDE", m)
	// Without this each re-executed child sleeps a second at exit under -race.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	x.Binary = func(string) (string, bool) { return bin, true }
}

func accountState(t *testing.T, e *env, label string) v1.AccountState {
	t.Helper()
	rows, err := e.store.ListAllAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Harness == "claude" && r.Label == label {
			return v1.AccountState(r.State)
		}
	}
	return ""
}

// The behaviour the whole state exists for: a turn that failed because the
// account has no working login leaves the account in needs-login.
//
// The failure itself never says so — Claude reports a missing login with
// is_error and terminal_reason "api_error" in the same object that says
// subtype "success", and reports a bad model identically — so the harness's
// own login check is what decides, not the wording.
func TestAFailedTurnWithNoLoginMarksTheAccountNeedsLogin(t *testing.T) {
	for _, c := range []struct {
		name         string
		credential   bool
		class        string
		before, want v1.AccountState
		rejected     bool
	}{
		{"no login behind an unexplained failure", false, adapter.ClassHarness, v1.AccountFree, v1.AccountNeedsLogin, false},
		{"no login behind a harness that just exited", false, adapter.ClassHarnessExited, v1.AccountFree, v1.AccountNeedsLogin, false},
		// A bad model fails the same way and must not cost the account its
		// state: the login is there, so the check says so.
		{"a failure with the login intact", true, adapter.ClassHarness, v1.AccountFree, v1.AccountFree, false},
		// A token account's login check says yes for any token, so the
		// provider refusing the credential is what parks it (decision 0054).
		{"a refused credential behind a login check that says yes", true, adapter.ClassHarness, v1.AccountFree, v1.AccountNeedsLogin, true},
		// A usage limit says why it failed. Reading it as a login problem
		// would hide DEV-27's state behind this one.
		{"a usage limit is not a login problem", false, adapter.ClassUsageLimit, v1.AccountFree, v1.AccountFree, false},
		{"a prompt that does not fit", false, adapter.ClassPromptTooLong, v1.AccountFree, v1.AccountFree, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			home, err := account.Ensure(e.paths.Data, "claude", "work")
			if err != nil {
				t.Fatal(err)
			}
			if c.credential {
				if err := os.WriteFile(filepath.Join(home, ".credentials.json"), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := account.SetState(ctx, e.store.Queries, "claude", "work", c.before, time.Now()); err != nil {
				t.Fatal(err)
			}
			l := e.loop(t, 1)
			e.enqueue(t, testRun("a", "s1"))
			x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
				Outcome: adapter.Outcome{State: v1.RunFailed, Error: &v1.RunError{Class: c.class, Message: "it failed"}, AuthRejected: c.rejected},
			}))
			fakeClaudeBinary(t, x)
			claimAndRun(t, l, x)

			if got := accountState(t, e, "work"); got != c.want {
				t.Errorf("account state = %q, want %q", got, c.want)
			}
		})
	}
}

// An account already in needs-login is never given a run, so no run can take
// it back out of that state. The way back is the owner's: `yad account add`
// runs the harness's login again and re-checks. Remote completion is DEV-57.
func TestNeedsLoginIsOnlyClearedByLoggingInAgain(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	home, err := account.Ensure(e.paths.Data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	// The owner logged in by hand; the store has not heard.
	if err := os.WriteFile(filepath.Join(home, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := account.SetState(ctx, e.store.Queries, "claude", "work", v1.AccountNeedsLogin, time.Now()); err != nil {
		t.Fatal(err)
	}
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunSucceeded},
	}))
	fakeClaudeBinary(t, x)
	claimAndRun(t, l, x)

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunFailed {
		t.Fatalf("the run was given a needs-login account: %+v", res)
	}
	if got := accountState(t, e, "work"); got != v1.AccountNeedsLogin {
		t.Errorf("account state = %q, want it to stand until the owner logs in", got)
	}
	if !strings.Contains(res.Error.Message, "yad --profile default account add claude work") {
		t.Errorf("the failure does not name the way back: %q", res.Error.Message)
	}
}

// A run that succeeded is never a reason to ask: the login obviously works.
func TestASuccessfulRunLeavesTheAccountAlone(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := account.Ensure(e.paths.Data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if err := account.SetState(ctx, e.store.Queries, "claude", "work", v1.AccountFree, time.Now()); err != nil {
		t.Fatal(err)
	}
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunSucceeded},
	}))
	fakeClaudeBinary(t, x)
	claimAndRun(t, l, x)

	if got := accountState(t, e, "work"); got != v1.AccountFree {
		t.Errorf("a successful run moved the account to %q", got)
	}
}

// health reads account state through account.Load, the same call a claim
// makes, so what a hub is told and what a run would do cannot disagree. These
// tests drive that path rather than hand-setting states on the document.
func healthLoop(t *testing.T, e *env, labels ...string) *Loop {
	t.Helper()
	l := e.loop(t, 1)
	l.Accounts = accountsOf(e.paths.Data, accountConfig(labels...))
	return l
}

// Every hub hears which accounts need login, by label, in the health of every
// sync — that is how an owner finds out from the hub's side why a runner is
// not claiming.
func TestHealthReportsNeedsLoginWithItsLabel(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, label := range []string{"personal", "work"} {
		if _, err := account.Ensure(e.paths.Data, "claude", label); err != nil {
			t.Fatal(err)
		}
	}
	if err := account.SetState(ctx, e.store.Queries, "claude", "work", v1.AccountNeedsLogin, time.Now()); err != nil {
		t.Fatal(err)
	}
	l := healthLoop(t, e, "personal", "work")

	h := l.health(ctx, heldNothing(l.Pool.Reserve(l.Connection)))
	if len(h.Harnesses) != 1 || h.Harnesses[0].ID != "claude" {
		t.Fatalf("health harnesses = %+v", h.Harnesses)
	}
	got := map[string]v1.AccountState{}
	for _, a := range h.Harnesses[0].Accounts {
		got[a.Label] = a.State
	}
	if got["work"] != v1.AccountNeedsLogin {
		t.Errorf("health says work is %q, want needs_login", got["work"])
	}
	// The state the store has never heard of, whose home is on disk, is free.
	if got["personal"] != v1.AccountFree {
		t.Errorf("health says personal is %q, want free", got["personal"])
	}
	if !h.Harnesses[0].Ready {
		t.Error("a harness with one free account is reported not ready")
	}
	// The home and everything in it stays out of health.
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), e.paths.Data) {
		t.Errorf("health carries a path into the data directory: %s", b)
	}
}

// The contradiction round 2 found: an account whose home is gone read as
// needs_login everywhere that went through account.Load — the capability
// document, `yad account list`, the claim — while health rebuilt the state
// itself and called the same label free and ready in the same sync. A hub
// routing on health kept offering runs that the claim then refused.
func TestHealthReportsAHomeThatIsGoneAsNeedsLogin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Exactly what `yad account remove` leaves behind for a daemon still
	// holding the config it started with: label configured, home deleted,
	// and no row in the store.
	l := healthLoop(t, e, "work")

	h := l.health(ctx, heldNothing(l.Pool.Reserve(l.Connection)))
	if len(h.Harnesses) != 1 || len(h.Harnesses[0].Accounts) != 1 {
		t.Fatalf("health harnesses = %+v", h.Harnesses)
	}
	if got := h.Harnesses[0].Accounts[0].State; got != v1.AccountNeedsLogin {
		t.Errorf("health says the account is %q; its home is not on disk", got)
	}
	if h.Harnesses[0].Ready {
		t.Error("a harness whose only account has no home is reported ready")
	}
	// And health agrees with what a run would actually do.
	accounts, err := l.Accounts.Load(ctx, e.store.Queries, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := account.Soonest(accounts, "claude", time.Now()); ok {
		t.Error("health and the claim disagree: a run would have taken the account")
	}
}

// Every account needing login makes the harness not ready, and the reason is
// there by label.
func TestAHarnessWhoseAccountsAllNeedLoginIsNotReady(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := account.Ensure(e.paths.Data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if err := account.SetState(ctx, e.store.Queries, "claude", "work", v1.AccountNeedsLogin, time.Now()); err != nil {
		t.Fatal(err)
	}
	l := healthLoop(t, e, "work")

	h := l.health(ctx, heldNothing(l.Pool.Reserve(l.Connection)))
	if len(h.Harnesses) != 1 || h.Harnesses[0].Ready {
		t.Fatalf("harness health = %+v, want not ready", h.Harnesses)
	}
}

// A harness with no accounts is ready: it runs on its own login, and having
// none configured is a state rather than a failure.
func TestAHarnessWithNoAccountsIsReady(t *testing.T) {
	e := newEnv(t)
	l := healthLoop(t, e)
	h := l.health(context.Background(), heldNothing(l.Pool.Reserve(l.Connection)))
	if len(h.Harnesses) != 1 || !h.Harnesses[0].Ready {
		t.Fatalf("harness health = %+v, want ready", h.Harnesses)
	}
	if len(h.Harnesses[0].Accounts) != 0 {
		t.Errorf("accounts = %+v, want none", h.Harnesses[0].Accounts)
	}
}

// Health reports what the runner can drive, by the same predicate a claim
// uses. A harness on PATH whose version probe failed is present and still
// unusable, and a hub routing on health must not be told it is ready.
func TestHealthLeavesOutAHarnessTheRunnerCannotDrive(t *testing.T) {
	e := newEnv(t)
	l := healthLoop(t, e)
	doc := l.Capabilities()
	doc.Harnesses[0].Error = "`claude --version` printed nothing this runner could parse"
	l.Capabilities = func() v1.Capabilities { return doc }

	h := l.health(context.Background(), heldNothing(l.Pool.Reserve(l.Connection)))
	if len(h.Harnesses) != 0 {
		t.Errorf("health reports %+v for a harness the runner would refuse a run for", h.Harnesses)
	}
}

// An unanswered login check leaves the account exactly as it was — one of the
// invariants the change is correct only if it holds, and the direction the
// login check's own comments argue for at length.
//
// Deleted by accident in round 2, which replaced the tail of this file rather
// than appending to it, and restored here: without it, inverting checkLogin's
// error branch leaves the whole suite green while an older claude with no
// `auth status` subcommand parks every account it touches.
func TestALoginCheckThatCannotAnswerLeavesTheStateAlone(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := account.Ensure(e.paths.Data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if err := account.SetState(ctx, e.store.Queries, "claude", "work", v1.AccountFree, time.Now()); err != nil {
		t.Fatal(err)
	}
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, logged := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunFailed, Error: &v1.RunError{Class: adapter.ClassHarness, Message: "it failed"}},
	}))
	fakeClaudeBinary(t, x, "broken")
	claimAndRun(t, l, x)

	if got := accountState(t, e, "work"); got != v1.AccountFree {
		t.Errorf("an unanswered login check moved the account to %q", got)
	}
	// And it is not silent about it: the owner has to be able to find out.
	if !strings.Contains(logged.String(), "could not check whether the account is still logged in") {
		t.Errorf("nothing was logged about the check failing:\n%s", logged.String())
	}
}

// A refusal is held against the token the turn was handed. An owner who
// stores a new token and then revokes the old one has turns still running on
// the old one; their refusal must not park the account on the token that now
// works (decision 0054).
func TestARefusalOfAReplacedTokenParksNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	home, err := account.Ensure(e.paths.Data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if err := account.SetToken(home, "sk-ant-oat01-old"); err != nil {
		t.Fatal(err)
	}
	_, used := account.TurnEnv("claude", home)
	if err := account.SetToken(home, "sk-ant-oat01-new"); err != nil {
		t.Fatal(err)
	}
	if err := account.SetState(ctx, e.store.Queries, "claude", "work", v1.AccountFree, time.Now()); err != nil {
		t.Fatal(err)
	}
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{}))
	fakeClaudeBinary(t, x)
	a := account.Account{Harness: "claude", Label: "work", Home: home, State: v1.AccountFree}
	res := v1.Result{State: v1.RunFailed, Error: &v1.RunError{Class: adapter.ClassHarness, Message: "401"}}
	log := slog.New(slog.DiscardHandler)

	x.checkLogin(ctx, a, "", res, true, used, log)
	if got := accountState(t, e, "work"); got != v1.AccountFree {
		t.Errorf("a refusal of the replaced token left the account %q, want free", got)
	}
	// The same refusal of the token still stored parks it.
	_, current := account.TurnEnv("claude", home)
	x.checkLogin(ctx, a, "", res, true, current, log)
	if got := accountState(t, e, "work"); got != v1.AccountNeedsLogin {
		t.Errorf("a refusal of the stored token left the account %q, want needs_login", got)
	}
}

// The error a hub sees when every account needs login names the command that
// fixes each kind: the plain login for a login account, --token - for a token
// account, whose token a plain login could not replace (decision 0054).
func TestNoFreeAccountNamesTheLoginThatFixesIt(t *testing.T) {
	e := newEnv(t)
	tokenHome, err := account.Ensure(e.paths.Data, "claude", "tl")
	if err != nil {
		t.Fatal(err)
	}
	if err := account.SetToken(tokenHome, "sk-ant-oat01-x"); err != nil {
		t.Fatal(err)
	}
	loginHome, err := account.Ensure(e.paths.Data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		label, home, want string
	}{
		{"tl", tokenHome, "account add claude tl --token -"},
		{"work", loginHome, "account add claude work`"},
	} {
		msg := (&noFreeAccountError{paths: e.paths, harness: "claude", accounts: []account.Account{
			{Harness: "claude", Label: c.label, Home: c.home, State: v1.AccountNeedsLogin},
		}}).Error()
		if !strings.Contains(msg, c.want) {
			t.Errorf("%s: %q does not name %q", c.label, msg, c.want)
		}
		if strings.Contains(msg, "sk-ant") {
			t.Errorf("%s: the error carries the token: %q", c.label, msg)
		}
	}
}
