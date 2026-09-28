package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// What the device-code fixtures answer with (codextest, codex 0.157.1's
// schema): the link and the code the owner is shown, and what Codex says when
// it will not log the account in — which must never reach a hub.
const (
	codexDeviceURL   = "https://auth.openai.com/codex/device"
	codexDeviceCode  = "K7QM-4XPD"
	codexRefusalText = "Device code login is not enabled"
	codexFixtures    = "../adapter/codex/testdata/codex-0.157.1/"
)

// deviceRig is a loginRig whose codex is the fake app-server playing a
// device-code login, held before its completion until open.
type deviceRig struct {
	*loginRig
	gate, codexLog, pid string
}

func newDeviceRig(t *testing.T, e *env, cfg config.Config, fixture string) *deviceRig {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The name is how the re-executed test binary knows to be codex.
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.Symlink(self, bin); err != nil {
		t.Fatal(err)
	}
	p, err := filepath.Abs(codexFixtures + fixture + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	d := &deviceRig{gate: filepath.Join(dir, "open"), codexLog: filepath.Join(dir, "codex.log"), pid: filepath.Join(dir, "pid")}
	t.Setenv("CODEX_TEST_FIXTURE", p)
	t.Setenv("CODEX_TEST_LOG", d.codexLog)
	t.Setenv("CODEX_TEST_GATE", d.gate)
	t.Setenv("CODEX_TEST_PID", d.pid)
	t.Setenv("CODEX_TEST_WAIT", "30s")
	d.loginRig = newLoginRig(t, e, cfg, "")
	codexBin := func(string) (string, bool) { return bin, true }
	d.Binary, d.Accounts.Binary = codexBin, codexBin
	// A failed test does not leave the fake waiting at its gate.
	t.Cleanup(d.open)
	return d
}

func (d *deviceRig) open() { os.WriteFile(d.gate, nil, 0o600) }

// sent is every message the runner wrote to the fake app-server.
func (d *deviceRig) sent(t *testing.T) []map[string]any {
	t.Helper()
	f, err := os.Open(d.codexLog)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e struct {
			Stdin *string `json:"stdin"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Stdin != nil {
			var m map[string]any
			json.Unmarshal([]byte(*e.Stdin), &m)
			out = append(out, m)
		}
	}
	return out
}

func (d *deviceRig) sentMethod(t *testing.T, method string) bool {
	t.Helper()
	for _, m := range d.sent(t) {
		if m["method"] == method {
			return true
		}
	}
	return false
}

// gone says the fake app-server's process is no more.
func (d *deviceRig) gone(t *testing.T) bool {
	t.Helper()
	b, err := os.ReadFile(d.pid)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return syscall.Kill(pid, 0) != nil
}

func codexConfig(labels ...string) config.Config {
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"codex": {Accounts: labels}}
	return cfg
}

func codexAccountState(t *testing.T, e *env, label string) v1.AccountState {
	t.Helper()
	rows, err := e.store.ListAllAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Harness == "codex" && r.Label == label {
			return v1.AccountState(r.State)
		}
	}
	return ""
}

func codexCredentialIn(home string) bool {
	_, err := os.Stat(filepath.Join(home, "auth.json"))
	return err == nil
}

// Codex from a hub (decision 0057): a link login whose waiting report
// carries the link and the code to type there, ended succeeded once Codex
// says the code was entered and its own check finds the login — for an
// account the owner listed, and for Codex's own default login. A login_code
// changes nothing: there is nothing to hand one to.
func TestAHubLogsCodexInByDeviceCode(t *testing.T) {
	for _, label := range []string{"work", ""} {
		t.Run("account "+label, func(t *testing.T) {
			e := newEnv(t)
			cfg := codexConfig("work")
			if label == "" {
				cfg = config.Default()
			}
			r := newDeviceRig(t, e, cfg, "login-device")
			r.Control("hub", startLogin("lg1", "codex", label))

			rep := r.until(t, "lg1", v1.LoginWaiting)
			if rep.URL != codexDeviceURL || rep.UserCode != codexDeviceCode {
				t.Fatalf("the report shows %q and %q, want codex's link and code", rep.URL, rep.UserCode)
			}
			if rep.Method != v1.LoginByLink || rep.Harness != "codex" || rep.Account != label {
				t.Errorf("the report is %+v", rep)
			}
			home := account.HomeDir(e.paths.Data, "codex", "work")
			if codexCredentialIn(home) {
				t.Fatal("the account was logged in before the code was entered")
			}
			r.Control("hub", loginCode("lg1", "a-code-no-codex-login-takes"))
			if rep := r.until(t, "lg1", v1.LoginWaiting); rep.State != v1.LoginWaiting || rep.UserCode != codexDeviceCode {
				t.Fatalf("a login_code moved the login: %+v", rep)
			}

			r.open()
			rep = r.until(t, "lg1", v1.LoginSucceeded)
			if rep.URL != "" || rep.UserCode != "" {
				t.Errorf("an ended login still shows its spent link or code: %+v", rep)
			}
			if r.changed.Load() == 0 {
				t.Error("nothing was told the login took, so the capability document waits for its next probe")
			}
			r.Close()
			for _, m := range r.sent(t) {
				if b, _ := json.Marshal(m); strings.Contains(string(b), "a-code-no-codex-login-takes") {
					t.Errorf("the hub's login_code reached codex: %s", b)
				}
			}
			if label == "" {
				if _, err := os.Stat(filepath.Join(e.paths.Data, "accounts")); !os.IsNotExist(err) {
					t.Errorf("a default login made account homes (%v)", err)
				}
				return
			}
			if !codexCredentialIn(home) {
				t.Error("codex's login wrote nothing in the account's home")
			}
			if got := codexAccountState(t, e, "work"); got != v1.AccountFree {
				t.Errorf("the account is %q after the login took, want free", got)
			}
			if strings.Contains(r.log.String(), codexDeviceCode) {
				t.Errorf("the code is in the runner's log:\n%s", r.log.String())
			}
		})
	}
}

// Every way a device-code login ends without taking, each in the runner's
// own words with what to do next, never Codex's: refused at OpenAI, a
// completion yad's own check does not believe, a codex with no device-code
// login, and no code entered within the deadline — which cancels Codex's own
// login too, so a code typed later logs nothing in.
func TestACodexDeviceLoginThatDoesNotTakeSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name, fixture, mode string
		want                v1.LoginState
		says                string
		cmds                [][]string
		cancelled           bool
	}{
		{name: "refused at OpenAI", fixture: "login-device-refused", want: v1.LoginFailed, says: "turn device-code login on",
			cmds: [][]string{{"yad", "--profile", "test", "account", "add", "codex", "work"}}},
		{name: "a completion codex's own check does not find", fixture: "login-device", mode: "nocred", want: v1.LoginFailed,
			says: "own check finds no login", cmds: [][]string{{"yad", "--profile", "test", "account", "add", "codex", "work"}}},
		{name: "a codex without device-code login", fixture: "login-device-unsupported", want: v1.LoginFailed,
			says: "would not start a device-code login", cmds: [][]string{
				{"yad", "--profile", "test", "doctor"}, {"yad", "--profile", "test", "account", "add", "codex", "work"}}},
		{name: "no code entered in time", fixture: "login-device-cancel", want: v1.LoginExpired,
			says: "not entered within", cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			t.Setenv("CODEX_TEST_LOGIN", tc.mode)
			r := newDeviceRig(t, e, codexConfig("work"), tc.fixture)
			r.CodeWait = 500 * time.Millisecond
			r.open()
			r.Control("hub", startLogin("lg1", "codex", "work"))
			rep := r.until(t, "lg1", tc.want)
			if !strings.Contains(rep.Error, tc.says) {
				t.Errorf("the error %q does not say %q", rep.Error, tc.says)
			}
			if strings.Contains(rep.Error, codexRefusalText) || strings.Contains(rep.Error, "unknown variant") || strings.Contains(rep.Error, e.paths.Data) {
				t.Errorf("the error quotes codex or names a path: %q", rep.Error)
			}
			cmds := shellwordtest.Commands(rep.Error, "yad ")
			if len(cmds) != len(tc.cmds) {
				t.Fatalf("want %d commands in %q", len(tc.cmds), rep.Error)
			}
			for i, want := range tc.cmds {
				shellwordtest.Check(t, cmds[i], want...)
			}
			r.Close()
			if got := r.sentMethod(t, "account/login/cancel"); got != tc.cancelled {
				t.Errorf("codex's own login was cancelled: %v, want %v", got, tc.cancelled)
			}
			if !r.gone(t) {
				t.Error("codex's app-server outlived the login")
			}
			if got := codexAccountState(t, e, "work"); got == v1.AccountFree {
				t.Error("a login that did not take left the account free")
			}
		})
	}
}

// A cancel_login, a newer login for the account, or the account removed at
// the machine: each ends the device-code login, cancels Codex's own and
// stops its app-server.
func TestACodexDeviceLoginEndedFromOutsideCancelsCodexsOwn(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, r *deviceRig)
		says string
	}{
		{"cancel_login", func(t *testing.T, r *deviceRig) {
			r.Control("hub", v1.Control{Kind: v1.ControlCancelLogin, LoginID: "lg1"})
		}, "cancelled from the hub"},
		{"the account removed", func(t *testing.T, r *deviceRig) {
			if _, err := r.Accounts.Reload(context.Background(), account.Lists{}, account.Ref{Harness: "codex", Label: "work"}, true); err != nil {
				t.Fatal(err)
			}
		}, "was removed from this runner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			r := newDeviceRig(t, e, codexConfig("work"), "login-device-cancel")
			r.Control("hub", startLogin("lg1", "codex", "work"))
			r.until(t, "lg1", v1.LoginWaiting)
			tc.end(t, r)
			if rep := r.until(t, "lg1", v1.LoginCancelled); !strings.Contains(rep.Error, tc.says) {
				t.Errorf("the login ended saying %q", rep.Error)
			}
			r.Close()
			if !r.sentMethod(t, "account/login/cancel") {
				t.Error("codex's own login was left waiting for the code")
			}
			if !r.gone(t) {
				t.Error("codex's app-server outlived the login")
			}
		})
	}
}

// A Codex account a hub adds rides the same device code (decision 0057):
// listed in config.toml and in the running lists only once Codex's own check
// finds the login, and free, so runs take it without a restart.
func TestAHubAddsACodexAccountByDeviceCode(t *testing.T) {
	e := newEnv(t)
	r := newDeviceRig(t, e, codexConfig("work"), "login-device")
	r.Control("hub", addLogin(startLogin("lg1", "codex", "second")))
	second := account.Ref{Harness: "codex", Label: "second"}

	if rep := r.until(t, "lg1", v1.LoginWaiting); rep.UserCode != codexDeviceCode {
		t.Fatalf("the report is %+v, want codex's code", rep)
	}
	if r.Accounts.Lists().Has(second) {
		t.Fatal("the account was listed before its login took")
	}

	r.open()
	r.until(t, "lg1", v1.LoginSucceeded)
	c, err := config.Load(e.paths)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Harness["codex"].Accounts; !slices.Equal(got, []string{"work", "second"}) {
		t.Errorf("config.toml lists %v, want the new account after the owner's", got)
	}
	if !r.Accounts.Lists().Has(second) {
		t.Error("the running lists do not name the new account, so no run takes it until a restart")
	}
	if got := codexAccountState(t, e, "second"); got != v1.AccountFree {
		t.Errorf("the added account is %q, want free", got)
	}
}
