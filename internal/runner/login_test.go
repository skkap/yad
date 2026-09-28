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
	"slices"
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
	// "nourl" printing no link, "hang" never exiting after the code,
	// "browser" as claude 2.1.282 on a machine whose browser is signed in to
	// claude.ai — signed in by that browser, no code, unless $BROWSER opens
	// nothing (DEV-133) — and "signedin" signing in that way whatever
	// $BROWSER says.
	fakeLoginMode = "RUNNER_TEST_LOGIN"
)

// fakeClaudeLogin is `claude auth login`, as the test binary re-executed.
func fakeClaudeLogin() int {
	mode := os.Getenv(fakeLoginMode)
	// Not claude's behaviour, a tripwire: a login handed the account's stored
	// token would be outranked by it, so a test sees one was.
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		fmt.Println("this fake refuses a login made with a token in its environment")
		return 1
	}
	fmt.Print("Opening browser to sign in…\n")
	// Measured on claude 2.1.282: with a browser signed in to claude.ai, the
	// link and the prompt are printed as usual, and then the browser's own
	// callback finishes the login with nothing read from stdin. `true` is the
	// one browser here that opens nothing.
	if mode == "signedin" || (mode == "browser" && os.Getenv("BROWSER") != "true") {
		fmt.Print("If the browser didn't open, visit: " + fakeLoginURL + "\n")
		fmt.Print("Paste code here if prompted > ")
		time.Sleep(300 * time.Millisecond)
		if err := os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json"), []byte(`{"claudeAiOauth":{"browser":true}}`), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("Login successful.")
		return 0
	}
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
	// config.toml says what the lists say, as on a machine: an account a hub
	// adds or removes is written there, and the lists read back from it.
	if err := config.Save(e.paths, cfg); err != nil {
		t.Fatal(err)
	}
	accounts := accountsOf(e.paths.Data, cfg)
	accounts.Binary = x.Binary
	accounts.attach(context.Background(), e.store)
	r := &loginRig{e: e, log: &syncBuffer{}}
	r.Logins = &Logins{
		Data: e.paths.Data, Paths: e.paths, Accounts: accounts, Binary: x.Binary,
		Changed: func() { r.changed.Add(1) },
		// As for a connection whose owner said nothing (decision 0057).
		MayManage: func(string) bool { return true },
		Log:       slog.New(slog.NewJSONHandler(r.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		URLWait:   20 * time.Second, CodeWait: 20 * time.Second, ExitWait: 20 * time.Second,
	}
	// As Serve wires it: an account the owner removes ends its login.
	accounts.onRemoved(r.accountRemoved)
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

// A hub login is signed in by the person at the hub, never by a browser on the
// runner's machine (DEV-133): the harness is handed one that opens nothing, so
// on a machine whose browser is signed in to claude.ai the login still waits
// for the code, and takes that one.
func TestAHubLinkLoginOpensNoBrowserOnTheMachine(t *testing.T) {
	// As on a desktop: no BROWSER of the owner's own.
	t.Setenv("BROWSER", "")
	e := newEnv(t)
	r := newLoginRig(t, e, accountConfig("work"), "browser")
	r.Control("hub", startLogin("lg1", "claude", "work"))
	r.until(t, "lg1", v1.LoginWaiting)
	home := account.HomeDir(e.paths.Data, "claude", "work")
	// Past the moment a signed-in browser would have finished it (the fake's
	// 300ms), the login is still waiting and the home still holds nothing.
	time.Sleep(time.Second)
	if credentialIn(home) {
		t.Fatal("the machine's browser signed the account in before any code arrived")
	}
	if rep := r.until(t, "lg1", v1.LoginWaiting); rep.State != v1.LoginWaiting {
		t.Fatalf("the login is %s, not still waiting for its code", rep.State)
	}
	r.Control("hub", loginCode("lg1", fakeLoginCode))
	r.until(t, "lg1", v1.LoginSucceeded)
}

// Should something on the machine sign the login in anyway, before any code,
// the report says so and that the account may be logged in as someone else —
// not a bare failure that leaves the owner thinking it is still logged out.
func TestALoginSignedInOnTheMachineSaysSo(t *testing.T) {
	e := newEnv(t)
	r := newLoginRig(t, e, accountConfig("work"), "signedin")
	r.Control("hub", startLogin("lg1", "claude", "work"))
	rep := r.until(t, "lg1", v1.LoginFailed)
	if !strings.Contains(rep.Error, "something on this machine signed it in") || !strings.Contains(rep.Error, "account list") {
		t.Errorf("the error %q does not say the account was signed in on the machine, or how to check it", rep.Error)
	}
	if !credentialIn(account.HomeDir(e.paths.Data, "claude", "work")) {
		t.Fatal("the fake did not sign the account in")
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
		{"no exit after the code", "hang", true, v1.LoginFailed, "had not finished"},
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

// A link login on a token account leaves the token to the account's runs
// while it is made, and is made and checked without it — handed the token,
// the login would be outranked and claude's check would say yes to anything.
// The token goes once the login takes, and stays when it does not.
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
			// Until the new login is confirmed, the account still runs on its
			// token: a run placed on it meanwhile is handed it.
			if env, _ := account.TurnEnv("claude", home); !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "CLAUDE_CODE_OAUTH_TOKEN=") }) {
				t.Errorf("a run during the login is handed no token: %v", env)
			}
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
// account the owner did not list, store a token with no account, or store
// one for Codex, which runs on none. None of them makes a home.
func TestALoginTheRunnerRefusesSaysWhatToDoAtTheMachine(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    v1.Control
		says string
		want []string
	}{
		{"an account nobody listed", startLogin("lg1", "claude", "stranger"), "start the login again with add",
			[]string{"yad", "--profile", "test", "account", "add", "claude", "stranger"}},
		{"a token with no account", v1.Control{Kind: v1.ControlLoginToken, LoginID: "lg1", Harness: "claude", Token: "sk-ant-oat01-x"}, "names none",
			[]string{"yad", "--profile", "test", "account", "add", "claude", "<label>", "--token", "-"}},
		{"a token for codex", v1.Control{Kind: v1.ControlLoginToken, LoginID: "lg1", Harness: "codex", Account: "work", Token: "sk-proj-not-a-subscription"},
			"does not run on a stored token", nil},
		{"a codex account nobody listed", startLogin("lg1", "codex", "stranger"), "never adds",
			[]string{"yad", "--profile", "test", "account", "add", "codex", "stranger"}},
		{"a harness hub login does not log in", startLogin("lg1", "gemini", "work"), "logs in claude",
			[]string{"yad", "--profile", "test", "doctor"}},
		// A name the hub sent is quoted in the words, and a backtick in it
		// must not read as the start of a second command.
		{"a harness named with a backtick", startLogin("lg1", "x`yad account list`", ""), "logs in claude",
			[]string{"yad", "--profile", "test", "doctor"}},
		{"no harness at all", startLogin("lg1", "", "work"), "naming one: hub login logs in claude and codex", nil},
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

// An account the owner removes while a hub login is on it: the login ends
// cancelled saying so, and the home goes once the login has let go of it,
// with whatever the login wrote there — whether the removal lands while the
// link waits for its code, or while a token login waits for the login it
// replaced to let go, and whether or not the daemon told the login.
func TestRemovingTheAccountEndsItsHubLogin(t *testing.T) {
	removed := func(t *testing.T, r *loginRig) {
		t.Helper()
		if _, err := r.Accounts.Reload(context.Background(), account.Lists{}, account.Ref{Harness: "claude", Label: "work"}, true); err != nil {
			t.Fatal(err)
		}
	}
	gone := func(t *testing.T, e *env) {
		t.Helper()
		if _, err := os.Stat(account.HomeDir(e.paths.Data, "claude", "work")); !os.IsNotExist(err) {
			t.Errorf("the removed account's home is still on disk (%v)", err)
		}
	}
	t.Run("while the link waits for its code", func(t *testing.T) {
		e := newEnv(t)
		r := newLoginRig(t, e, accountConfig("work"), "")
		r.Control("hub", startLogin("lg1", "claude", "work"))
		r.until(t, "lg1", v1.LoginWaiting)
		removed(t, r)
		rep := r.until(t, "lg1", v1.LoginCancelled)
		if !strings.Contains(rep.Error, "removed") {
			t.Errorf("the login ended saying %q", rep.Error)
		}
		cmds := shellwordtest.Commands(rep.Error, "yad ")
		if len(cmds) != 1 {
			t.Fatalf("want one command in %q", rep.Error)
		}
		shellwordtest.Check(t, cmds[0], "yad", "--profile", "test", "account", "add", "claude", "work")
		r.Close()
		gone(t, e)
	})
	for _, told := range []bool{true, false} {
		t.Run(fmt.Sprintf("while a token login waits, told %v", told), func(t *testing.T) {
			e := newEnv(t)
			r := newLoginRig(t, e, accountConfig("work"), "")
			if !told {
				r.Accounts.onRemoved(nil)
			}
			// A login still letting go of the home, standing in for one the
			// token login replaces.
			r.init()
			prev := &hubLogin{conn: "hub", id: "prev", ref: account.Ref{Harness: "claude", Label: "work"}, method: v1.LoginByLink,
				state: v1.LoginWaiting, updated: time.Now(), done: make(chan struct{})}
			r.mu.Lock()
			r.live[prev.ref] = prev
			r.mu.Unlock()
			r.Control("hub", v1.Control{Kind: v1.ControlLoginToken, LoginID: "lg2", Harness: "claude", Account: "work", Token: "sk-ant-oat01-for-a-removed-account"})
			removed(t, r)
			close(prev.done)
			if rep := r.until(t, "lg2", v1.LoginCancelled); !strings.Contains(rep.Error, "removed") {
				t.Errorf("the login ended saying %q", rep.Error)
			}
			r.Close()
			gone(t, e)
		})
	}
}

// An account first logged in its own way and later put on a token still has
// its old credential beside the token. A link login on it counts only when
// claude's own login succeeded with this code — a refusal, or a login still
// asking when the deadline comes, is a failure whatever claude's check says of
// the old credential — so a mistyped code neither reports success nor removes
// the token the owner chose. The right code still takes, and the token goes.
func TestALoginThatRefusedItsCodeFailsWhateverTheHomeHolds(t *testing.T) {
	for _, tc := range []struct {
		name, mode, code string
		want             v1.LoginState
		keepToken        bool
	}{
		{"a code claude refuses", "", "not-the-code", v1.LoginFailed, true},
		{"a login still asking at the deadline", "hang", "not-the-code", v1.LoginFailed, true},
		{"the right code", "", fakeLoginCode, v1.LoginSucceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			r := newLoginRig(t, e, accountConfig("work"), tc.mode)
			r.ExitWait = 500 * time.Millisecond
			home := plantCredential(t, e.paths.Data, "work")
			if err := account.SetToken(home, "sk-ant-oat01-the-owners-choice"); err != nil {
				t.Fatal(err)
			}
			if err := account.SetState(ctx, e.store.Queries, "claude", "work", v1.AccountNeedsLogin, time.Now()); err != nil {
				t.Fatal(err)
			}
			r.Control("hub", startLogin("lg1", "claude", "work"))
			r.until(t, "lg1", v1.LoginWaiting)
			r.Control("hub", loginCode("lg1", tc.code))
			r.until(t, "lg1", tc.want)
			r.Close()
			if got := account.HasToken(home); got != tc.keepToken {
				t.Errorf("the account has its token: %v, want %v", got, tc.keepToken)
			}
			want := v1.AccountNeedsLogin
			if tc.want == v1.LoginSucceeded {
				want = v1.AccountFree
			}
			if got := accountState(t, e, "work"); got != want {
				t.Errorf("the account is %q, want %q", got, want)
			}
		})
	}
}

// A login cancelled or replaced is still in the account's home until its
// goroutine has let go — its process being stopped, its check running — and
// the next login for the account waits for it, whichever order the hub's
// answer carries the cancel and the start in. Here the old login is held
// mid-teardown, and the new one must not write its token until it lets go.
func TestANewLoginWaitsForTheOldOneToLetGoOfTheHome(t *testing.T) {
	cancelOld := v1.Control{Kind: v1.ControlCancelLogin, LoginID: "old"}
	startNew := v1.Control{Kind: v1.ControlLoginToken, LoginID: "new", Harness: "claude", Account: "work", Token: "sk-ant-oat01-the-new-one"}
	for _, tc := range []struct {
		name  string
		order []v1.Control
	}{
		{"cancel then start", []v1.Control{cancelOld, startNew}},
		{"start then cancel", []v1.Control{startNew, cancelOld}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			r := newLoginRig(t, e, accountConfig("work"), "")
			home, err := account.Ensure(e.paths.Data, "claude", "work")
			if err != nil {
				t.Fatal(err)
			}
			r.init()
			old := &hubLogin{conn: "hub", id: "old", ref: account.Ref{Harness: "claude", Label: "work"}, method: v1.LoginByLink,
				state: v1.LoginWaiting, updated: time.Now(), code: make(chan string, 1), done: make(chan struct{}), cancel: func() {}}
			r.mu.Lock()
			r.byKey[loginKey{"hub", "old"}], r.live[old.ref] = old, old
			r.mu.Unlock()
			for _, c := range tc.order {
				r.Control("hub", c)
			}
			r.until(t, "old", v1.LoginCancelled)
			time.Sleep(300 * time.Millisecond)
			if account.TokenFileExists(home) {
				t.Fatal("the new login wrote its token while the old one was still in the home")
			}
			for _, rep := range r.Reports("hub") {
				if rep.LoginID == "new" && rep.State != v1.LoginStarting {
					t.Fatalf("the new login moved to %s before the old one let go", rep.State)
				}
			}
			close(old.done)
			r.until(t, "new", v1.LoginSucceeded)
			if !account.HasToken(home) {
				t.Error("the new login stored no token once the old one let go")
			}
		})
	}
}

// A hub login makes the account's home before its harness can write there, and
// an account with a home and no row reads free (account.Load). A login that
// ended any way but succeeded therefore left an account listed by hand, never
// logged in, free — ready in health, offered runs, failing each (DEV-138). How
// the login ends must not decide the account's state: the harness's own check
// does, as it does for one that took. Health is read after a sweep of the
// login probe, which moves nothing to needs_login and so cannot hide the
// state a login left; `yad account list` and `yad doctor` read the store
// through account.Read.
func TestALoginThatEndsWithoutTakingLeavesTheAccountAsItsCheckSays(t *testing.T) {
	cancel := func(t *testing.T, r *loginRig) {
		r.until(t, "lg1", v1.LoginWaiting)
		r.Control("hub", v1.Control{Kind: v1.ControlCancelLogin, LoginID: "lg1"})
	}
	for _, tc := range []struct {
		name string
		// home is what the account had before the hub's login began: none,
		// as for a label listed by hand, "empty" for a home whose login is
		// gone, or "logged in" at the machine.
		home string
		// check is RUNNER_TEST_CLAUDE for the login's own check: "broken"
		// cannot answer, and "once" answers the login and not the check
		// that follows it.
		check string
		start v1.Control
		then  func(t *testing.T, r *loginRig)
		ends  v1.LoginState
		want  v1.AccountState
	}{
		{"cancelled from the hub", "", "", startLogin("lg1", "claude", "work"), cancel, v1.LoginCancelled, v1.AccountNeedsLogin},
		{"expired", "", "", startLogin("lg1", "claude", "work"), nil, v1.LoginExpired, v1.AccountNeedsLogin},
		{"failed on a wrong code", "", "", startLogin("lg1", "claude", "work"), func(t *testing.T, r *loginRig) {
			r.until(t, "lg1", v1.LoginWaiting)
			r.Control("hub", loginCode("lg1", "not-the-code"))
		}, v1.LoginFailed, v1.AccountNeedsLogin},
		{"a token whose check cannot answer", "", "broken",
			v1.Control{Kind: v1.ControlLoginToken, LoginID: "lg1", Harness: "claude", Account: "work", Token: "sk-ant-oat01-unchecked"},
			nil, v1.LoginFailed, v1.AccountNeedsLogin},
		{"taken", "", "", startLogin("lg1", "claude", "work"), func(t *testing.T, r *loginRig) {
			r.until(t, "lg1", v1.LoginWaiting)
			r.Control("hub", loginCode("lg1", fakeLoginCode))
		}, v1.LoginSucceeded, v1.AccountFree},
		{"taken, and the check after it cannot answer", "", "once",
			v1.Control{Kind: v1.ControlLoginToken, LoginID: "lg1", Harness: "claude", Account: "work", Token: "sk-ant-oat01-checked-once"},
			nil, v1.LoginSucceeded, v1.AccountFree},
		// The check, not the end: an account already logged in keeps its
		// login when a hub's attempt at another one is abandoned.
		{"cancelled on an account logged in at the machine", "logged in", "", startLogin("lg1", "claude", "work"), cancel, v1.LoginCancelled, v1.AccountFree},
		// A home with no login read free before the login, and its check
		// says what a run would have found.
		{"cancelled on a home whose login is gone", "empty", "", startLogin("lg1", "claude", "work"), cancel, v1.LoginCancelled, v1.AccountNeedsLogin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			cfg := accountConfig("work")
			lists := account.ListsOf(cfg)
			r := newLoginRig(t, e, cfg, "")
			r.CodeWait = time.Second
			switch tc.home {
			case "empty":
				if _, err := account.Ensure(e.paths.Data, "claude", "work"); err != nil {
					t.Fatal(err)
				}
			case "logged in":
				plantCredential(t, e.paths.Data, "work")
			}
			if tc.check != "" {
				t.Setenv("RUNNER_TEST_CLAUDE", tc.check)
			}
			before := v1.AccountFree
			if tc.home == "" {
				before = v1.AccountNeedsLogin
			}
			if got := listed(t, e, lists); got != before {
				t.Fatalf("before the login the account is %q, want %q", got, before)
			}

			r.Control("hub", tc.start)
			if tc.then != nil {
				tc.then(t, r)
			}
			r.until(t, "lg1", tc.ends)
			// Close waits for the login to let go of the home.
			r.Close()

			probe := &LoginProbe{Store: e.store, Accounts: r.Accounts, Binary: r.Binary}
			probe.Sweep(ctx)
			l := healthLoop(t, e, "work")
			l.Accounts = r.Accounts
			h := l.health(ctx, heldNothing(l.Pool.Reserve(l.Connection)))
			if len(h.Harnesses) != 1 || len(h.Harnesses[0].Accounts) != 1 {
				t.Fatalf("health harnesses = %+v", h.Harnesses)
			}
			if got := h.Harnesses[0].Accounts[0].State; got != tc.want {
				t.Errorf("health says the account is %q after a login %s, want %q", got, tc.ends, tc.want)
			}
			if ready := h.Harnesses[0].Ready; ready != (tc.want == v1.AccountFree) {
				t.Errorf("health says the harness is ready: %v, with its one account %q", ready, tc.want)
			}
			if got := listed(t, e, lists); got != tc.want {
				t.Errorf("`yad account list` says the account is %q, health %q", got, tc.want)
			}
		})
	}
}

// A login still in flight is not over, and the account's state on disk must
// already be the one it leaves if the daemon goes now. Killed while the link
// waits for its code, a daemon unwinds nothing, so the needs_login a missing
// home meant is written before the home is made. Stopped cleanly, the daemon
// ends every login in flight through the same release as an end from the hub,
// and the check decides: here a home whose login is gone, which read free
// before the login, is found needing one.
func TestALoginInFlightLeavesTheAccountAsItWouldBeFound(t *testing.T) {
	t.Run("killed while the link waits", func(t *testing.T) {
		e := newEnv(t)
		cfg := accountConfig("work")
		r := newLoginRig(t, e, cfg, "")
		r.Control("hub", startLogin("lg1", "claude", "work"))
		r.until(t, "lg1", v1.LoginWaiting)
		if _, err := os.Stat(account.HomeDir(e.paths.Data, "claude", "work")); err != nil {
			t.Fatalf("the login made no home (%v)", err)
		}
		if got := listed(t, e, account.ListsOf(cfg)); got != v1.AccountNeedsLogin {
			t.Errorf("while the login waits the account reads %q; a daemon killed now restarts with it so", got)
		}
	})
	t.Run("stopped while the link waits", func(t *testing.T) {
		e := newEnv(t)
		cfg := accountConfig("work")
		r := newLoginRig(t, e, cfg, "")
		if _, err := account.Ensure(e.paths.Data, "claude", "work"); err != nil {
			t.Fatal(err)
		}
		if got := listed(t, e, account.ListsOf(cfg)); got != v1.AccountFree {
			t.Fatalf("before the login the account is %q, want free", got)
		}
		r.Control("hub", startLogin("lg1", "claude", "work"))
		r.until(t, "lg1", v1.LoginWaiting)
		r.Close()
		if got := listed(t, e, account.ListsOf(cfg)); got != v1.AccountNeedsLogin {
			t.Errorf("after the daemon stopped mid-login the account is %q, want needs_login", got)
		}
	})
}

// listed is the account's state as `yad account list` and `yad doctor` read
// it: from state.db, read-only, through account.Read.
func listed(t *testing.T, e *env, lists account.Lists) v1.AccountState {
	t.Helper()
	accounts, err := account.Read(context.Background(), e.paths, lists, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("account.Read = %+v, want the one account", accounts)
	}
	return accounts[0].State
}
