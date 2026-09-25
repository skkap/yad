package runner

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// What claude 2.1.281 prints for `claude auth login` over pipes, measured on
// a work machine: the link, then a prompt it reads the code after.
const (
	fakeLoginURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile&code_challenge=Zc3a&code_challenge_method=S256&state=q9Xw"
	// fakeLoginCode is the one code the fake takes; the credential is written
	// for it and nothing else.
	fakeLoginCode = "Kq7-right-code#state-q9Xw"
	// fakeLoginMode picks how the fake's login behaves: "" as measured,
	// "osc8" with the link in a terminal hyperlink as a TTY would get it,
	// "nourl" printing no link, "hang" never exiting after the code.
	fakeLoginMode = "RUNNER_TEST_LOGIN"
)

// fakeClaudeLogin is `claude auth login`, as the test binary re-executed.
func fakeClaudeLogin() int {
	mode := os.Getenv(fakeLoginMode)
	fmt.Print("Opening browser to sign in…\n")
	switch mode {
	case "nourl":
		io.Copy(io.Discard, os.Stdin)
		return 1
	case "osc8":
		fmt.Print("If the browser didn't open, visit: \x1b]8;;" + fakeLoginURL + "\x1b\\" + fakeLoginURL + "\x1b]8;;\x1b\\\n")
	default:
		fmt.Print("If the browser didn't open, visit: " + fakeLoginURL + "\n")
	}
	fmt.Print("Paste code here if prompted > ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return 1
	}
	if mode == "hang" {
		time.Sleep(time.Hour)
	}
	if strings.TrimSpace(line) != fakeLoginCode {
		fmt.Println("Invalid code. Please make sure the full code was copied.")
		return 1
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json"), []byte(`{"claudeAiOauth":{}}`), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("Login successful.")
	return 0
}

// loginRig is a Logins over the env with the fake claude, its log kept.
type loginRig struct {
	*Logins
	e       *env
	log     *syncBuffer
	changed atomic.Int32
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newLoginRig(t *testing.T, e *env, cfg config.Config, mode string) *loginRig {
	t.Helper()
	x := &Exec{}
	fakeClaudeBinary(t, x)
	t.Setenv(fakeLoginMode, mode)
	accounts := accountsOf(e.paths.Data, cfg)
	accounts.Binary = x.Binary
	accounts.attach(context.Background(), e.store)
	r := &loginRig{e: e, log: &syncBuffer{}}
	r.Logins = &Logins{
		Data: e.paths.Data, Paths: e.paths, Accounts: accounts, Binary: x.Binary,
		Changed: func() { r.changed.Add(1) },
		Log:     slog.New(slog.NewJSONHandler(r.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		URLWait: 20 * time.Second, CodeWait: 20 * time.Second, ExitWait: 20 * time.Second,
	}
	t.Cleanup(r.Close)
	return r
}

// until waits for the login to reach a state and returns its report.
func (r *loginRig) until(t *testing.T, id string, want v1.LoginState) v1.LoginReport {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last v1.LoginReport
	for time.Now().Before(deadline) {
		for _, rep := range r.Reports("hub") {
			if rep.LoginID == id {
				last = rep
				if rep.State == want {
					return rep
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("login %s is %+v, never %s\nlog:\n%s", id, last, want, r.log.String())
	return last
}

func startLogin(id, harness, label string) v1.Control {
	return v1.Control{Kind: v1.ControlStartLogin, LoginID: id, Harness: harness, Account: label}
}

func loginCode(id, code string) v1.Control {
	return v1.Control{Kind: v1.ControlLoginCode, LoginID: id, Code: code}
}

func credentialIn(home string) bool {
	_, err := os.Stat(filepath.Join(home, ".credentials.json"))
	return err == nil
}

// The link way, end to end on the runner's side: claude's own login run in the
// account's home over pipes, its link reported as it printed it — plainly, or
// inside the hyperlink escape a terminal would get — the owner's code handed
// to it, and the account in service because claude's own check says so.
func TestAHubLogsAListedAccountInByLink(t *testing.T) {
	for _, mode := range []string{"", "osc8"} {
		t.Run("output "+mode, func(t *testing.T) {
			e := newEnv(t)
			r := newLoginRig(t, e, accountConfig("work"), mode)
			r.Control("hub", startLogin("lg1", "claude", "work"))

			rep := r.until(t, "lg1", v1.LoginWaiting)
			if rep.URL != fakeLoginURL {
				t.Fatalf("the link reported is %q, want claude's own:\n%s", rep.URL, fakeLoginURL)
			}
			if rep.Method != v1.LoginByLink || rep.Harness != "claude" || rep.Account != "work" {
				t.Errorf("the report is %+v", rep)
			}
			home := account.HomeDir(e.paths.Data, "claude", "work")
			if credentialIn(home) {
				t.Fatal("the account was logged in before any code arrived")
			}
			r.Control("hub", loginCode("lg1", fakeLoginCode))
			r.until(t, "lg1", v1.LoginSucceeded)
			if !credentialIn(home) {
				t.Error("claude's login wrote nothing in the account's home")
			}
			if got := accountState(t, e, "work"); got != v1.AccountFree {
				t.Errorf("the account is %q after the login took, want free", got)
			}
			if r.changed.Load() == 0 {
				t.Error("nothing was told the login took, so the capability document waits for its next probe")
			}
		})
	}
}

// A code claude refuses is a failed login, decided by claude's own check and
// not by what it printed; the account is left as it was.
func TestAWrongCodeFailsTheLogin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := newLoginRig(t, e, accountConfig("work"), "")
	if err := account.SetState(ctx, e.store.Queries, "claude", "work", v1.AccountNeedsLogin, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.Control("hub", startLogin("lg1", "claude", "work"))
	r.until(t, "lg1", v1.LoginWaiting)
	r.Control("hub", loginCode("lg1", "not-the-code"))
	rep := r.until(t, "lg1", v1.LoginFailed)
	if !strings.Contains(rep.Error, "start a new login") {
		t.Errorf("the error %q does not say what to do", rep.Error)
	}
	if rep.URL != "" {
		t.Errorf("a failed login still shows its spent link %q", rep.URL)
	}
	if got := accountState(t, e, "work"); got != v1.AccountNeedsLogin {
		t.Errorf("a failed login left the account %q, want needs_login", got)
	}
}

// The deadlines: a login that prints no link fails saying so, and one whose
// code never comes expires — each with its process gone.
func TestALinkLoginEndsAtItsDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		code       bool
		want       v1.LoginState
		says       string
	}{
		{"no link", "nourl", false, v1.LoginFailed, "printed no link"},
		{"no code", "", false, v1.LoginExpired, "no code arrived"},
		{"no exit after the code", "hang", true, v1.LoginFailed, "finds no login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			r := newLoginRig(t, e, accountConfig("work"), tc.mode)
			r.URLWait, r.CodeWait, r.ExitWait = 500*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond
			r.Control("hub", startLogin("lg1", "claude", "work"))
			if tc.code {
				r.until(t, "lg1", v1.LoginWaiting)
				r.Control("hub", loginCode("lg1", fakeLoginCode))
			}
			rep := r.until(t, "lg1", tc.want)
			if !strings.Contains(rep.Error, tc.says) {
				t.Errorf("the error %q does not say %q", rep.Error, tc.says)
			}
			// Close waits for the goroutine, which waits for the process.
			r.Close()
		})
	}
}

// One login per account: a second start supersedes the first, which ends
// cancelled naming its successor, and the second one is the login that takes.
// A cancel ends a login; one for a login this runner never had is answered
// cancelled all the same, so the hub stops asking.
func TestANewLoginSupersedesAndACancelEnds(t *testing.T) {
	e := newEnv(t)
	r := newLoginRig(t, e, accountConfig("work"), "")
	r.Control("hub", startLogin("lg1", "claude", "work"))
	r.until(t, "lg1", v1.LoginWaiting)
	r.Control("hub", startLogin("lg2", "claude", "work"))
	old := r.until(t, "lg1", v1.LoginCancelled)
	if !strings.Contains(old.Error, "lg2") {
		t.Errorf("the superseded login says %q, not which login replaced it", old.Error)
	}
	r.until(t, "lg2", v1.LoginWaiting)
	// A code for the superseded login goes nowhere.
	r.Control("hub", loginCode("lg1", fakeLoginCode))
	r.Control("hub", v1.Control{Kind: v1.ControlCancelLogin, LoginID: "lg2"})
	r.until(t, "lg2", v1.LoginCancelled)
	if credentialIn(account.HomeDir(e.paths.Data, "claude", "work")) {
		t.Error("a cancelled login logged the account in")
	}
	r.Control("hub", v1.Control{Kind: v1.ControlCancelLogin, LoginID: "never"})
	r.until(t, "never", v1.LoginCancelled)
	// A code for a login this runner does not have — as after a restart.
	r.Control("hub", loginCode("gone", fakeLoginCode))
	if rep := r.until(t, "gone", v1.LoginFailed); !strings.Contains(rep.Error, "restarted") {
		t.Errorf("a code for an unknown login says %q", rep.Error)
	}
}

// The harness's own default login, for a harness with no account: logged in
// where its runs look, with no home of yad's made.
func TestAHubLogsTheDefaultLoginIn(t *testing.T) {
	e := newEnv(t)
	def := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", def)
	r := newLoginRig(t, e, config.Default(), "")
	r.Control("hub", startLogin("lg1", "claude", ""))
	r.until(t, "lg1", v1.LoginWaiting)
	r.Control("hub", loginCode("lg1", fakeLoginCode))
	r.until(t, "lg1", v1.LoginSucceeded)
	if !credentialIn(def) {
		t.Error("the default login was not written to the harness's own home")
	}
	if _, err := os.Stat(filepath.Join(e.paths.Data, "accounts")); !os.IsNotExist(err) {
		t.Errorf("a default login made account homes (%v)", err)
	}
}

// The token way: the token stored as the account's login (decision 0054) and
// the account in service; nothing else of the machine changes.
func TestAHubStoresATokenForAListedAccount(t *testing.T) {
	e := newEnv(t)
	r := newLoginRig(t, e, accountConfig("work"), "")
	const tok = "sk-ant-oat01-hub-delivered-token"
	r.Control("hub", v1.Control{Kind: v1.ControlLoginToken, LoginID: "lg1", Harness: "claude", Account: "work", Token: tok})
	rep := r.until(t, "lg1", v1.LoginSucceeded)
	if rep.Method != v1.LoginByToken {
		t.Errorf("the report says method %q", rep.Method)
	}
	home := account.HomeDir(e.paths.Data, "claude", "work")
	if !account.HasToken(home) {
		t.Fatal("the token is not stored in the account's home")
	}
	if got := accountState(t, e, "work"); got != v1.AccountFree {
		t.Errorf("the account is %q, want free", got)
	}
	if strings.Contains(r.log.String(), tok) {
		t.Errorf("the token is in the log:\n%s", r.log.String())
	}
}

// A link login on a token account sets the token aside, as `yad account add`
// does: gone once the login takes, back when it does not.
func TestALinkLoginOnATokenAccountKeepsTheTokenUnlessItTakes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      string
		keepToken bool
	}{
		{"it takes", fakeLoginCode, false},
		{"it does not", "wrong", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			r := newLoginRig(t, e, accountConfig("work"), "")
			home, err := account.Ensure(e.paths.Data, "claude", "work")
			if err != nil {
				t.Fatal(err)
			}
			if err := account.SetToken(home, "sk-ant-oat01-old"); err != nil {
				t.Fatal(err)
			}
			r.Control("hub", startLogin("lg1", "claude", "work"))
			r.until(t, "lg1", v1.LoginWaiting)
			r.Control("hub", loginCode("lg1", tc.code))
			want := v1.LoginSucceeded
			if tc.keepToken {
				want = v1.LoginFailed
			}
			r.until(t, "lg1", want)
			r.Close()
			if got := account.HasToken(home); got != tc.keepToken {
				t.Errorf("the account has a token: %v, want %v", got, tc.keepToken)
			}
		})
	}
}

// What a runner will not do, each ended failed at once with the reason and a
// command for whoever walks to the machine, runnable as printed: log in an
// account the owner did not list, store a token with no account, or log
// Codex in from a hub. None of them makes a home.
func TestALoginTheRunnerRefusesSaysWhatToDoAtTheMachine(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    v1.Control
		says string
		want []string
	}{
		{"an account nobody listed", startLogin("lg1", "claude", "stranger"), "never adds",
			[]string{"yad", "--profile", "test", "account", "add", "claude", "stranger"}},
		{"a token with no account", v1.Control{Kind: v1.ControlLoginToken, LoginID: "lg1", Harness: "claude", Token: "sk-ant-oat01-x"}, "names none",
			[]string{"yad", "--profile", "test", "account", "add", "claude", "<label>", "--token", "-"}},
		{"codex", startLogin("lg1", "codex", "work"), "not built yet",
			[]string{"yad", "--profile", "test", "account", "add", "codex", "work", "--device"}},
		{"a token that is not one", v1.Control{Kind: v1.ControlLoginToken, LoginID: "lg1", Harness: "claude", Account: "work", Token: "two words"}, "not stored", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			cfg := accountConfig("work")
			cfg.Harness["codex"] = config.HarnessConfig{Accounts: []string{"work"}}
			r := newLoginRig(t, e, cfg, "")
			r.Control("hub", tc.c)
			rep := r.until(t, "lg1", v1.LoginFailed)
			if !strings.Contains(rep.Error, tc.says) {
				t.Errorf("the error %q does not say %q", rep.Error, tc.says)
			}
			if strings.Contains(rep.Error, e.paths.Data) || strings.Contains(rep.Error, tc.c.Token) && tc.c.Token != "" {
				t.Errorf("the error carries a path or the token: %q", rep.Error)
			}
			if tc.want != nil {
				cmds := shellwordtest.Commands(rep.Error, "yad ")
				if len(cmds) != 1 {
					t.Fatalf("want one command in %q", rep.Error)
				}
				shellwordtest.Check(t, cmds[0], tc.want...)
			}
			if _, err := os.Stat(filepath.Join(e.paths.Data, "accounts")); !os.IsNotExist(err) {
				t.Errorf("a refused login made a home (%v)", err)
			}
		})
	}
}

// A login is reported until a sync carrying its end is answered, and then
// forgotten; one still going stays however often it is reported. Each
// connection hears only of its own.
func TestALoginIsReportedUntilItsEndIsAnswered(t *testing.T) {
	e := newEnv(t)
	r := newLoginRig(t, e, accountConfig("work"), "")
	r.Control("hub", startLogin("lg1", "claude", "work"))
	r.Control("hub", v1.Control{Kind: v1.ControlCancelLogin, LoginID: "other"})
	r.until(t, "lg1", v1.LoginWaiting)
	if got := r.Reports("elsewhere"); len(got) != 0 {
		t.Errorf("another connection is told of this one's logins: %+v", got)
	}
	sent := r.Reports("hub")
	if len(sent) != 2 {
		t.Fatalf("reports %+v, want two", sent)
	}
	r.Reported("hub", sent)
	if got := r.Reports("hub"); len(got) != 1 || got[0].LoginID != "lg1" {
		t.Fatalf("after a sync carrying them was answered the runner reports %+v; only the ongoing lg1 should be left", got)
	}
	// Taken again: a repeated start is the same instruction.
	r.Control("hub", startLogin("lg1", "claude", "work"))
	if got := r.Reports("hub"); len(got) != 1 || got[0].State != v1.LoginWaiting {
		t.Errorf("a repeated start_login moved the login: %+v", got)
	}
}
