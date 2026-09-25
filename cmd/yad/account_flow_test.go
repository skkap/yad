package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
	"github.com/skkap/yad/internal/store"
)

// fakeClaudeLogin, set to "fails", makes the fake claude's `auth login` exit 1
// having written nothing: the owner walking away from the browser.
const fakeClaudeLogin = "E2E_CLAUDE_LOGIN"

// fakeClaudeAuth is `claude auth login` and `claude auth status`, keeping a
// stand-in credential in the home it is pointed at, as claude keeps one in
// CLAUDE_CONFIG_DIR. Its contents never matter: nothing in YAD reads them.
func fakeClaudeAuth(cmd string) {
	cred := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json")
	switch cmd {
	case "login":
		if os.Getenv(fakeClaudeLogin) == "fails" {
			os.Stderr.WriteString("login cancelled\n")
			os.Exit(1)
		}
		if err := os.WriteFile(cred, []byte(`{"stand_in":true}`), 0o600); err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
	case "status":
		// With no account's home, this is the harness's own default home,
		// which every test that is not about logins wants logged in: the
		// runner refuses runs for a harness whose own login is missing
		// (decision 0053).
		in := os.Getenv("CLAUDE_CONFIG_DIR") == ""
		if !in {
			_, err := os.Stat(cred)
			in = err == nil
		}
		// As claude does: any CLAUDE_CODE_OAUTH_TOKEN is a login to its
		// check, valid or not.
		if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
			in = true
		}
		b, _ := json.Marshal(map[string]any{"loggedIn": in})
		os.Stdout.Write(append(b, '\n'))
		// As claude 2.1.281 does: a "no" exits 1 as well.
		if !in {
			os.Exit(1)
		}
	default:
		os.Exit(2)
	}
}

// atTerminal stands the owner at the terminal `yad account add` insists on.
func atTerminal(t *testing.T) {
	t.Helper()
	old := interactive
	interactive = func() bool { return true }
	t.Cleanup(func() { interactive = old })
}

// accountHarness is a profile of the test's own with h's fake installed, for
// the account commands with no daemon.
func accountHarness(t *testing.T, h *e2eHarness) config.Paths {
	t.Helper()
	p := accountEnv(t)
	h.install(t)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	atTerminal(t)
	return p
}

func noStateDB(t *testing.T, p config.Paths) {
	t.Helper()
	if _, err := os.Stat(p.StateDB()); !os.IsNotExist(err) {
		t.Errorf("the CLI made or touched the state database %s (%v); it is the daemon's alone", p.StateDB(), err)
	}
}

// The ownership rule by machine rather than by review: no CLI path under
// cmd/yad opens the runner's state database for writing (decision 0043). The
// daemon is its only writer; the CLI reads it read-only (store.OpenProfile) or
// asks the daemon over the control socket. `yad hub` opens a database of its
// own through a package of its own, which is why the import path is what is
// matched and not the package's name.
func TestNoCLIPathOpensTheStateDatabaseForWriting(t *testing.T) {
	const runnerStore = "github.com/skkap/yad/internal/store"
	writers := []string{"Open", "OpenSQLite"}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		local := ""
		for _, imp := range f.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); path == runnerStore {
				local = "store"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if local == "" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == local && slices.Contains(writers, sel.Sel.Name) {
				t.Errorf("%s: %s.%s opens the runner's state database read-write from the CLI — ask the daemon over the control socket, or read with store.OpenProfile", fset.Position(sel.Pos()), local, sel.Sel.Name)
			}
			return true
		})
	}
}

// A login that did not take adds nothing: config.toml is as it was, the home
// is kept so the next try picks it up, the exit is non-zero, and the error's
// next action is the same command again, runnable as printed.
func TestAccountAddWithAFailedLoginChangesNothing(t *testing.T) {
	eachHarness(t, func(t *testing.T, h *e2eHarness) {
		for _, tc := range []struct {
			name string
			// listed is an account already in config.toml whose login the
			// owner is redoing; it must stay exactly where it was.
			listed []string
		}{
			{name: "a new account"},
			{name: "an account already listed", listed: []string{"work"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				p := accountHarness(t, h)
				t.Setenv(h.login, "fails")
				cfg := config.Default()
				if tc.listed != nil {
					cfg.Harness = map[string]config.HarnessConfig{h.name: {Accounts: tc.listed}}
				}
				if err := config.Save(p, cfg); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(p.ConfigFile())
				if err != nil {
					t.Fatal(err)
				}

				code, out, errs := yadIn(t, "account", "add", h.name, "work")
				if code == 0 {
					t.Fatalf("a failed login exited 0:\n%s", out)
				}
				after, err := os.ReadFile(p.ConfigFile())
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(before) {
					t.Errorf("config.toml changed:\n%s\nwas:\n%s", after, before)
				}
				if _, err := os.Stat(account.HomeDir(p.Data, h.name, "work")); err != nil {
					t.Errorf("the home was not kept for a retry: %v", err)
				}
				noStateDB(t, p)
				shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad --profile default account add"), dirsEnv(t),
					"yad", "--profile", "default", "account", "add", h.name, "work")
			})
		}
	})
}

// A login that took, with no daemon running: the label is in config.toml, the
// state database is not written — not even created — and the account reads
// free, because its home is on disk and nothing says otherwise.
func TestAccountAddWithNoDaemonAddsTheLabelAndWritesNoState(t *testing.T) {
	eachHarness(t, func(t *testing.T, h *e2eHarness) {
		p := accountHarness(t, h)
		code, out, errs := yadIn(t, "account", "add", h.name, "work")
		if code != 0 {
			t.Fatalf("exit %d: %s\n%s", code, errs, out)
		}
		cfg, err := config.Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Harness[h.name].Accounts; !slices.Equal(got, []string{"work"}) {
			t.Errorf("accounts = %v, want [work]", got)
		}
		noStateDB(t, p)
		shellwordtest.CheckEnv(t, onlyCommand(t, out, "yad --profile default daemon"), dirsEnv(t),
			"yad", "--profile", "default", "daemon", "start")

		code, out, errs = yadIn(t, "account", "list", "--json")
		if code != 0 {
			t.Fatalf("list: exit %d: %s", code, errs)
		}
		var reps []struct {
			Harness  string             `json:"harness"`
			Accounts []v1.AccountReport `json:"accounts"`
		}
		if err := json.Unmarshal([]byte(out), &reps); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		if len(reps) != 1 || reps[0].Harness != h.name || len(reps[0].Accounts) != 1 ||
			reps[0].Accounts[0].Label != "work" || reps[0].Accounts[0].State != v1.AccountFree {
			t.Errorf("list = %+v, want %s work free", reps, h.name)
		}
		noStateDB(t, p)
	})
}

// Removing an account with no daemon running deletes its home and takes it
// out of config.toml, and writes nothing to the state database: its rows stay
// until a daemon starts and prunes them (internal/runner).
func TestAccountRemoveWithNoDaemonLeavesTheStateDatabaseAlone(t *testing.T) {
	p := accountEnv(t)
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"work"}}}
	if err := config.Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := account.Ensure(p.Data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	recordLimit(t, p, "work", reset, reset)
	before, err := os.ReadFile(p.StateDB())
	if err != nil {
		t.Fatal(err)
	}

	code, out, errs := yadIn(t, "account", "remove", "claude", "work", "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("output = %q", out)
	}
	if _, err := os.Stat(account.HomeDir(p.Data, "claude", "work")); !os.IsNotExist(err) {
		t.Errorf("the home survived: %v", err)
	}
	after, err := os.ReadFile(p.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the CLI wrote to the state database; with no daemon running it writes nothing")
	}
	st, err := store.OpenReadOnly(context.Background(), p.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if rows, err := st.ListAllAccounts(context.Background()); err != nil || len(rows) != 1 {
		t.Errorf("rows = %+v (%v), want the removed account's row left for the daemon", rows, err)
	}
}

// With a daemon running, add and remove take effect without a restart: a new
// account takes the next run, a run already on an account the owner removes
// finishes on it — in a home still on disk — and the removed account takes no
// new run. Its home goes when that run lets it go.
func TestE2EAccountsChangeWithoutARestart(t *testing.T) { eachHarness(t, testE2EAccountsChange) }

func testE2EAccountsChange(t *testing.T, h *e2eHarness) {
	m := newMachine(t, h)
	d := m.daemon()
	ctx := context.Background()
	watchOK := func(runID string) {
		t.Helper()
		if code, out, errs := m.watch(runID); code != 0 {
			t.Fatalf("watch %s: exit %d: %s\n%s\ndaemon:\n%s", runID, code, errs, out, d.out.String())
		}
	}
	// Opened once the daemon has migrated it, after the first run: two
	// processes migrating one database at once is not a thing this tests.
	var s *store.Store
	accountOf := func(runID string) string {
		t.Helper()
		r := localRun(t, s, runID)
		if !r.Account.Valid {
			return ""
		}
		return r.Account.String
	}
	home := account.HomeDir(m.p.data, h.name, "work")

	// No account yet: the harness's own default home, and a daemon that is
	// up and syncing before anything changes.
	m.submit("acct-0")
	watchOK("acct-0")
	s = m.runnerStore()
	if got := accountOf("acct-0"); got != "" {
		t.Fatalf("acct-0 ran on account %q before any was added", got)
	}

	atTerminal(t)
	out := m.ok("account", "add", h.name, "work")
	if !strings.Contains(out, "taken it up") {
		t.Errorf("add did not say the running daemon took the account up:\n%s", out)
	}
	m.submit("acct-1")
	watchOK("acct-1")
	if got := accountOf("acct-1"); got != "work" {
		t.Fatalf("acct-1 ran on account %q; the account added to a running daemon should take it", got)
	}

	m.gated()
	m.submit("acct-2")
	m.waitAtGate()
	out = m.ok("account", "remove", h.name, "work", "--yes")
	if !strings.Contains(out, "acct-2") {
		t.Errorf("remove did not name the run still on the account:\n%s", out)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("the home went while a run was on it: %v", err)
	}
	m.open()
	watchOK("acct-2")
	if got := accountOf("acct-2"); got != "work" {
		t.Errorf("acct-2 ran on %q, want work — it was on the account before the remove", got)
	}
	eventually(t, "the removed account's home is deleted once its last run ends", func() bool {
		_, err := os.Stat(home)
		return os.IsNotExist(err)
	})

	m.submit("acct-3")
	watchOK("acct-3")
	if got := accountOf("acct-3"); got != "" {
		t.Errorf("acct-3 ran on account %q, which the owner removed", got)
	}
	eventually(t, "the removed account's state is forgotten", func() bool {
		rows, err := s.ListAllAccounts(ctx)
		return err == nil && len(rows) == 0
	})
}

// A Claude account can be added with a `claude setup-token` token piped in:
// no terminal, no login in the account's home — a provisioning script can do
// it — and the token lives in the home, 0600, and reaches no output
// (decision 0054).
func TestAccountAddTakesATokenFromStdin(t *testing.T) {
	p := accountHarness(t, claudeE2E)
	interactive = func() bool { return false }
	const tok = "sk-ant-oat01-not-a-real-token"
	old := stdin
	stdin = strings.NewReader(tok + "\n")
	code, out, errs := yadIn(t, "account", "add", "claude", "tl", "--token", "-")
	stdin = old
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errs, out)
	}
	if strings.Contains(out+errs, tok) {
		t.Errorf("the token was printed:\n%s%s", out, errs)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Harness["claude"].Accounts; !slices.Equal(got, []string{"tl"}) {
		t.Errorf("accounts = %v, want [tl]", got)
	}
	home := account.HomeDir(p.Data, "claude", "tl")
	if got := account.Env("claude", home); len(got) != 2 || got[1] != "CLAUDE_CODE_OAUTH_TOKEN="+tok {
		t.Errorf("the account's runs would get %q", got)
	}
	noStateDB(t, p)

	code, out, _ = yadIn(t, "account", "list")
	if code != 0 || !strings.Contains(out, "token ") || strings.Contains(out, tok) {
		t.Errorf("list does not show a token account, or shows the token:\n%s", out)
	}
}

// The ways a token must not arrive, and the harnesses it does not fit.
func TestAccountAddRefusesATokenItCannotUse(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"account", "add", "claude", "tl", "--token", "sk-ant-oat01-x"}, "never from the command line"},
		{[]string{"account", "add", "codex", "tl", "--token", "-"}, "--device"},
		{[]string{"account", "add", "claude", "tl", "--device"}, "claude setup-token"},
	} {
		p := accountEnv(t)
		code, out, errs := yadIn(t, c.args...)
		if code == 0 || !strings.Contains(errs, c.want) {
			t.Errorf("%q: exit %d, stderr %q, want a refusal naming %q", c.args, code, errs, c.want)
		}
		if strings.Contains(out+errs, "sk-ant-oat01-x") && !strings.Contains(c.args[len(c.args)-1], "sk-ant") {
			t.Errorf("%q echoed a token", c.args)
		}
		if _, err := os.Stat(account.HomeDir(p.Data, c.args[2], "tl")); err == nil {
			t.Errorf("%q made the account's home before refusing", c.args)
		}
	}
}
