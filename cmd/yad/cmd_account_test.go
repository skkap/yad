package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
)

// accountEnv is a profile whose directories the test owns, and the paths to
// reach into them with.
func accountEnv(t *testing.T) config.Paths {
	t.Helper()
	cfgDir, dataDir := t.TempDir(), shortDir(t)
	t.Setenv("YAD_CONFIG_DIR", cfgDir)
	t.Setenv("YAD_DATA_DIR", dataDir)
	t.Setenv("PATH", t.TempDir())
	return config.Paths{Profile: config.DefaultProfile, Config: cfgDir, Data: dataDir}
}

// yadIn runs a yad command against an environment the caller already set up,
// so several commands can share one profile.
func yadIn(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestAccountListWithNoAccounts(t *testing.T) {
	accountEnv(t)
	code, out, errs := yadIn(t, "account", "list")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	// Absence is data: a runner with no accounts says so and says what to do.
	if !strings.Contains(out, "no accounts") || !strings.Contains(out, "yad account add") {
		t.Errorf("output = %q", out)
	}
}

// What `yad account list` shows the owner, and what its JSON gives a script:
// the label and the state, never the home or anything inside it.
func TestAccountListShowsStateAndKeepsTheHomeOutOfItsJSON(t *testing.T) {
	p := accountEnv(t)
	ctx := context.Background()
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"personal", "work"}}}
	if err := config.Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	if err := recordState(ctx, p, "claude", "work", v1.AccountNeedsLogin); err != nil {
		t.Fatal(err)
	}

	code, out, errs := yadIn(t, "account", "list")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"personal", "free", "work", "needs_login", "yad account add claude work"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing does not mention %q:\n%s", want, out)
		}
	}

	code, out, errs = yadIn(t, "account", "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var reps []v1.HarnessReport
	if err := json.Unmarshal([]byte(out), &reps); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(reps) != 1 || len(reps[0].Accounts) != 2 {
		t.Fatalf("reports = %+v", reps)
	}
	// The owner's order, not the alphabet's and not the store's.
	if reps[0].Accounts[0].Label != "personal" || reps[0].Accounts[1].Label != "work" {
		t.Errorf("order = %+v", reps[0].Accounts)
	}
	if reps[0].Accounts[1].State != v1.AccountNeedsLogin {
		t.Errorf("state = %q", reps[0].Accounts[1].State)
	}
	if strings.Contains(out, p.Data) {
		t.Errorf("the JSON carries the path to the account homes:\n%s", out)
	}
}

// A login needs a person at the terminal. Run from a script it must refuse
// rather than leave a home with no login in it and say nothing.
func TestAccountAddRefusesToRunUnattended(t *testing.T) {
	p := accountEnv(t)
	code, _, errs := yadIn(t, "account", "add", "claude", "work")
	if code == 0 {
		t.Fatal("an unattended `yad account add` succeeded")
	}
	if !strings.Contains(errs, "terminal") {
		t.Errorf("the refusal does not say why: %q", errs)
	}
	// And it made nothing: no half-built home, no entry in the config.
	if _, err := os.Stat(account.HomeDir(p.Data, "claude", "work")); !os.IsNotExist(err) {
		t.Errorf("a home was made anyway: %v", err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Harness["claude"].Accounts) != 0 {
		t.Errorf("the account was configured anyway: %v", cfg.Harness["claude"].Accounts)
	}
}

func TestAccountAddRefusesAHarnessWithNoHomeOfItsOwn(t *testing.T) {
	accountEnv(t)
	code, _, errs := yadIn(t, "account", "add", "gemini", "work")
	if code == 0 {
		t.Fatal("a harness yad has no account home for was accepted")
	}
	// The refusal names what does work rather than only what does not.
	if !strings.Contains(errs, "claude") || !strings.Contains(errs, "codex") {
		t.Errorf("the refusal does not say which harnesses have accounts: %q", errs)
	}
}

func TestAccountAddRefusesABadLabel(t *testing.T) {
	accountEnv(t)
	for _, label := range []string{"../escape", "Work", "a/b"} {
		code, _, errs := yadIn(t, "account", "add", "claude", label)
		if code == 0 {
			t.Errorf("label %q was accepted", label)
		}
		if !strings.Contains(errs, "label") {
			t.Errorf("label %q: %q", label, errs)
		}
	}
}

// Removing an account deletes its login and nothing else. The transcripts are
// shared by every account, and the link is deleted rather than followed.
func TestAccountRemoveDeletesTheHomeAndKeepsTheTranscripts(t *testing.T) {
	p := accountEnv(t)
	ctx := context.Background()
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"personal", "work"}}}
	if err := config.Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	home, err := account.Ensure(p.Data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if err := recordState(ctx, p, "claude", "work", v1.AccountNeedsLogin); err != nil {
		t.Fatal(err)
	}
	shared := account.TranscriptDir(p.Data, "claude")
	session := filepath.Join(shared, "a-session.jsonl")
	if err := os.WriteFile(session, []byte("a conversation"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errs := yadIn(t, "account", "remove", "claude", "work", "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("output = %q", out)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("the home survived: %v", err)
	}
	if b, err := os.ReadFile(session); err != nil || string(b) != "a conversation" {
		t.Errorf("the shared transcripts went with the account: %v", err)
	}
	// The other account still reaches them.
	other, err := account.Ensure(p.Data, "claude", "personal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "projects", "a-session.jsonl")); err != nil {
		t.Errorf("the remaining account lost the session: %v", err)
	}
	// It is out of the owner's order and out of the store.
	cfg, err = config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Harness["claude"].Accounts) != 1 || cfg.Harness["claude"].Accounts[0] != "personal" {
		t.Errorf("accounts = %v", cfg.Harness["claude"].Accounts)
	}
	accounts, err := account.Read(ctx, p, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		if a.Label == "work" {
			t.Errorf("the removed account is still in the store: %+v", a)
		}
	}
}

// Deleting a login is not undoable, so an unattended remove has to say so.
func TestAccountRemoveWithoutYesRefusesUnattended(t *testing.T) {
	p := accountEnv(t)
	home, err := account.Ensure(p.Data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	code, _, errs := yadIn(t, "account", "remove", "claude", "work")
	if code == 0 {
		t.Fatal("an unattended remove went ahead without being asked to")
	}
	if !strings.Contains(errs, "--yes") {
		t.Errorf("the refusal does not say how to mean it: %q", errs)
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("the home was removed anyway: %v", err)
	}
}

// `yad account use` is the ordering command, and ordering is failover's.
func TestAccountUseNamesTheTaskThatBringsIt(t *testing.T) {
	accountEnv(t)
	code, _, errs := yadIn(t, "account", "use", "claude", "work")
	if code == 0 || !strings.Contains(errs, "DEV-28") {
		t.Errorf("exit %d: %q", code, errs)
	}
}

func TestAccountUsageIsPrintedForNonsense(t *testing.T) {
	accountEnv(t)
	for _, args := range [][]string{{"account"}, {"account", "wat"}, {"account", "add", "claude"}} {
		code, _, errs := yadIn(t, args...)
		if code == 0 || !strings.Contains(errs, "usage") {
			t.Errorf("%v: exit %d, %q", args, code, errs)
		}
	}
}

// The state a needs-login account is recorded in survives being written and
// read back through the same paths the daemon uses.
func TestRecordedStateIsWhatTheDocumentReports(t *testing.T) {
	p := accountEnv(t)
	ctx := context.Background()
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"work"}}}
	if err := config.Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	if err := recordState(ctx, p, "claude", "work", v1.AccountNeedsLogin); err != nil {
		t.Fatal(err)
	}
	accounts, err := account.Read(ctx, p, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].State != v1.AccountNeedsLogin {
		t.Fatalf("accounts = %+v", accounts)
	}
	if accounts[0].UpdatedAt.IsZero() || time.Since(accounts[0].UpdatedAt) > time.Hour {
		t.Errorf("updated_at = %v", accounts[0].UpdatedAt)
	}
}
