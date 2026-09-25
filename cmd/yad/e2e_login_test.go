package main

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// What claude 2.1.281 prints for `claude auth login` over pipes, measured.
const (
	e2eLoginURL  = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&code_challenge=Zc3a&code_challenge_method=S256&state=q9Xw"
	e2eLoginCode = "Kq7-the-code-the-owner-pasted#q9Xw"
)

// fakeClaudeLinkLogin is the measured login: a link, a prompt, the code read
// from stdin, and a credential written only for the right one.
func fakeClaudeLinkLogin(cred string) {
	os.Stdout.WriteString("Opening browser to sign in…\nIf the browser didn't open, visit: " + e2eLoginURL + "\nPaste code here if prompted > ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != e2eLoginCode {
		os.Stdout.WriteString("Invalid code. Please make sure the full code was copied.\n")
		os.Exit(1)
	}
	if err := os.WriteFile(cred, []byte(`{"stand_in":true}`), 0o600); err != nil {
		os.Exit(1)
	}
	os.Stdout.WriteString("Login successful.\n")
}

func (m *machine) listAccounts(harness string, labels ...string) {
	m.t.Helper()
	p := config.Paths{Config: m.p.config, Data: m.p.data}
	cfg, err := config.Load(p)
	if err != nil {
		m.t.Fatal(err)
	}
	cfg.Harness = map[string]config.HarnessConfig{harness: {Accounts: labels}}
	if err := config.Save(p, cfg); err != nil {
		m.t.Fatal(err)
	}
}

// A hub login through the operator's commands against a running daemon: the
// link printed by `yad hub login start` and the code read from its stdin, a
// wrong code ending it failed, a token piped into `yad hub login token` — and
// neither the code nor the token anywhere on the runner's side it should not
// be, nor on any terminal.
func TestHubLoginThroughTheCLI(t *testing.T) {
	m := newMachine(t, claudeE2E)
	t.Setenv(fakeClaudeLogin, "link")
	old := loginPoll
	loginPoll = 20 * time.Millisecond
	t.Cleanup(func() { loginPoll = old })
	m.listAccounts("claude", "work", "spare")
	d := m.daemon()
	id := m.runnerID()
	var said strings.Builder

	out := m.ok2(e2eLoginCode+"\n", "hub", "login", "start", "--hub", m.service, id, "claude", "work")
	said.WriteString(out)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] != e2eLoginURL || !strings.Contains(lines[1], "is logged in") {
		t.Fatalf("start printed:\n%s\ndaemon:\n%s", out, d.out.String())
	}
	if _, err := os.Stat(filepath.Join(account.HomeDir(m.p.data, "claude", "work"), ".credentials.json")); err != nil {
		t.Errorf("claude's login wrote nothing in the account's home: %v", err)
	}
	if list := m.ok("account", "list", "--json"); !strings.Contains(strings.Join(strings.Fields(list), ""), `"label":"work","state":"free"`) {
		t.Errorf("the account is not free after the login took:\n%s", list)
	}

	code, out, errs := m.p.yad("not-the-code\n", "hub", "login", "start", "--code", "-", "--hub", m.service, id, "claude", "spare")
	said.WriteString(out + errs)
	if code == 0 || !strings.Contains(errs, "ended failed") {
		t.Errorf("a wrong code: exit %d\n%s\n%s", code, out, errs)
	}
	if strings.Contains(errs, "paste the code you are given") {
		t.Errorf("--code - asked for the code anyway: %s", errs)
	}

	const tok = "sk-ant-oat01-piped-into-the-hub"
	code, out, errs = m.p.yad(tok+"\n", "hub", "login", "token", "--hub", m.service, id, "claude", "spare")
	said.WriteString(out + errs)
	if code != 0 || !strings.Contains(out, "is logged in") {
		t.Fatalf("token: exit %d\n%s\n%s\ndaemon:\n%s", code, out, errs, d.out.String())
	}
	if !account.HasToken(account.HomeDir(m.p.data, "claude", "spare")) {
		t.Error("the token is not stored in the account's home")
	}

	// A paste that is not one token is refused before it leaves the machine,
	// with the command to pipe it into, runnable as printed.
	code, _, errs = m.p.yad("two words\n", "hub", "login", "token", "--hub", m.service, id, "claude", "spare")
	if code == 0 {
		t.Fatal("a token of two words was sent")
	}
	shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad --profile default hub login token"), dirsEnv(t),
		"yad", "--profile", "default", "hub", "login", "token", "--hub", m.service, id, "claude", "spare")

	said.WriteString(d.out.String())
	for _, secret := range []string{e2eLoginCode, "not-the-code", tok} {
		if strings.Contains(said.String(), secret) {
			t.Errorf("a secret was printed:\n%s", said.String())
		}
		filepath.WalkDir(m.p.data, func(path string, e fs.DirEntry, err error) error {
			// hub.db is this test's hub, still open; the store's own test
			// proves what its file keeps once it has closed.
			if err != nil || !e.Type().IsRegular() || filepath.Base(path) == "yad-oauth-token" || strings.HasPrefix(filepath.Base(path), "hub.db") {
				return nil
			}
			if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), secret) {
				t.Errorf("%s holds a secret", path)
			}
			return nil
		})
	}
}
