package capability

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// standIn installs a claude or codex that is a shell script, found through
// its override, and asked by the real adapter: the failure it plays is read
// the way a real harness's would be.
func standIn(t *testing.T, id, script string, mode os.FileMode) harness.Detected {
	t.Helper()
	noTools(t)
	bin := filepath.Join(t.TempDir(), id)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), mode); err != nil {
		t.Fatal(err)
	}
	d := ready(t, id)
	d.Path = bin
	t.Setenv(d.EnvPath, bin)
	return d
}

func resetModels(t *testing.T) {
	t.Helper()
	modelsMu.Lock()
	modelsAsked, modelsFailing = map[string]modelsAnswer{}, nil
	modelsMu.Unlock()
}

// A harness that cannot say which models it offers leaves a reason, in yad's
// words and with what to do, for the daemon's log and `yad doctor` — and the
// document says only that the list is the catalog's (DEV-146). Nothing the
// harness printed is in the reason: each stand-in says "someone", as a path
// under an owner's home would.
func TestModelsFailureSaysWhy(t *testing.T) {
	oldTimeout := modelsTimeout
	t.Cleanup(func() { modelsTimeout = oldTimeout })
	for _, tc := range []struct {
		name, id, script string
		mode             os.FileMode
		// want is in the reason; args, when set, is the command it names,
		// run through sh with the override standing in for the binary.
		want string
		args []string
	}{
		{"too old", "claude", "read line\n" +
			`echo '{"type":"control_response","response":{"subtype":"error","request_id":"yad-list-models","error":"Unsupported control request subtype: list_models at /Users/someone"}}'` + "\n",
			0o755, "as a Claude Code older than the request does (2.1.283 answers it)", []string{"update"}},
		{"codex too old", "codex", "read line\n" + `echo '{"id":1,"result":{}}'` + "\nread line\nread line\n" +
			`echo '{"id":2,"error":{"code":-32601,"message":"no model/list for /Users/someone"}}'` + "\n/bin/cat >/dev/null\n",
			0o755, "refused model/list, and what it said is not kept — a configuration or login it cannot load", []string{"login", "status"}},
		{"timeout", "claude", "echo 'waiting on /Users/someone' >&2\n/bin/cat >/dev/null\n", 0o755,
			"did not answer list_models within", []string{"--version"}},
		{"failed start", "claude", "echo someone\n", 0o644,
			"could not be started to ask it: YAD_CLAUDE_PATH does not name a claude this runner can start", nil},
		{"stopped", "claude", "echo 'panic at /Users/someone' >&2\nexit 1\n", 0o755,
			"stopped before it answered list_models, and what it printed is not kept", []string{"--version"}},
		{"no model", "claude", "read line\n" +
			`echo '{"type":"control_response","response":{"subtype":"success","request_id":"yad-list-models","response":{"models":[{"value":"/Users/someone"}]}}}'` + "\n",
			0o755, "with no model this yad can read", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetModels(t)
			modelsTimeout = 20 * time.Second
			if tc.name == "timeout" {
				modelsTimeout = 300 * time.Millisecond
			}
			t.Setenv("CODEX_HOME", t.TempDir())
			d := standIn(t, tc.id, tc.script, tc.mode)
			found := []harness.Detected{d}
			addModels(context.Background(), found, config.Default(), nil)

			got := ModelsFailures()
			if len(got) != 1 || got[0].Harness != tc.id || got[0].Account != "" || got[0].Since.IsZero() {
				t.Fatalf("failures = %+v, want one for %s's own login", got, tc.id)
			}
			reason := got[0].Reason
			if !strings.Contains(reason, tc.want) {
				t.Errorf("reason %q, want it to say %q", reason, tc.want)
			}
			if strings.Contains(reason, "someone") || strings.Contains(reason, d.Path) {
				t.Errorf("reason %q carries what the harness printed or where it is", reason)
			}
			if tc.args != nil {
				prefix := `"$` + d.EnvPath + `" `
				cmds := shellwordtest.Commands(reason, prefix)
				if len(cmds) != 1 {
					t.Fatalf("reason %q, want one command starting %s", reason, prefix)
				}
				shellwordtest.Check(t, d.EnvPath+"=stub\n"+cmds[0], append([]string{"stub"}, tc.args...)...)
				if !strings.Contains(reason, d.EnvPath+" set in that shell") {
					t.Errorf("reason %q does not say %s must be set where it is pasted", reason, d.EnvPath)
				}
			}

			// The document is as it was: the list is the catalog's, or none
			// for Codex, and nothing says why.
			rep := Harnesses(found, config.Default(), nil)[0]
			if tc.id == "claude" && rep.ModelsSource != v1.ModelsFromCatalog {
				t.Errorf("models from %q, want the catalog", rep.ModelsSource)
			}
			b, _ := json.Marshal(rep)
			if strings.Contains(string(b), reason) || strings.Contains(string(b), tc.want) {
				t.Errorf("the document carries the reason: %s", b)
			}
		})
	}
}

// An account's failure points its check at that account's home: without it
// `codex login status` reads the owner's default login, which may load fine,
// and sends them looking for a cause that is not there.
func TestModelsFailureChecksTheLoginAsked(t *testing.T) {
	resetModels(t)
	d := standIn(t, "codex", "read line\n"+`echo '{"id":1,"result":{}}'`+"\nread line\nread line\n"+
		`echo '{"id":2,"error":{"code":-32600,"message":"failed to load configuration"}}'`+"\n/bin/cat >/dev/null\n", 0o755)
	home := filepath.Join(t.TempDir(), "it's work")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"codex": {Accounts: []string{"work"}}}
	accounts := []account.Account{{Harness: "codex", Label: "work", Home: home, State: v1.AccountFree}}
	addModels(context.Background(), []harness.Detected{d}, cfg, accounts)

	got := ModelsFailures()
	if len(got) != 1 || got[0].Account != "work" {
		t.Fatalf("failures = %+v, want account work's", got)
	}
	prefix := `"$` + d.EnvPath + `" `
	cmds := shellwordtest.Commands(got[0].Reason, prefix)
	if len(cmds) != 1 {
		t.Fatalf("reason %q, want one command starting %s", got[0].Reason, prefix)
	}
	shellwordtest.CheckEnv(t, d.EnvPath+"=stub\n"+cmds[0], map[string]string{"CODEX_HOME": home}, "stub", "login", "status")
}

// A failure is kept beside the answer it leaves standing, per login: an
// account that answers says nothing, one that does not is named, and its
// reason keeps the time it began while it stays the same. A new reason starts
// again, an answer clears it, and so does a forget — a new login is asked
// afresh.
func TestModelsFailureIsKeptPerLogin(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	modelsNow = func() time.Time { return now }
	t.Cleanup(func() { modelsNow = time.Now })
	work, home := t.TempDir(), t.TempDir()
	var failWith error
	a := &asker{answer: func(_, h string) ([]string, error) {
		if h == home && failWith != nil {
			return nil, failWith
		}
		return []string{"opus"}, nil
	}}
	a.install(t)
	resetModels(t)
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"work", "home"}}}
	accounts := []account.Account{
		{Harness: "claude", Label: "work", Home: work, State: v1.AccountFree},
		{Harness: "claude", Label: "home", Home: home, State: v1.AccountFree},
	}
	build := func() []ModelsFailure {
		found := []harness.Detected{ready(t, "claude")}
		addModels(context.Background(), found, cfg, accounts)
		return ModelsFailures()
	}
	refused := adapter.ModelsError(adapter.ErrModelsRefused, "refused", nil)
	unread := adapter.ModelsError(adapter.ErrModelsUnread, "none", nil)
	start := now
	steps := []struct {
		name    string
		advance time.Duration
		fail    error
		forget  bool
		// since is how long after start the failure began; negative is none.
		since time.Duration
		want  string
	}{
		{"answered", 0, nil, false, -1, ""},
		{"stale, and refused", modelsRecheck, refused, false, modelsRecheck, "refused list_models"},
		{"kept while it waits to retry", time.Minute, refused, false, modelsRecheck, "refused list_models"},
		{"refused again", modelsRetry, refused, false, modelsRecheck, "refused list_models"},
		{"a new reason", modelsRetry, unread, false, modelsRecheck + time.Minute + 2*modelsRetry, "no model this yad can read"},
		{"answered again", modelsRetry, nil, false, -1, ""},
		{"refused, then forgotten", modelsRecheck, refused, false, 2*modelsRecheck + time.Minute + 3*modelsRetry, "refused"},
		{"asked afresh", 0, nil, true, -1, ""},
	}
	for _, s := range steps {
		now = now.Add(s.advance)
		failWith = s.fail
		if s.forget {
			ForgetModels("claude")
		}
		got := build()
		if s.since < 0 {
			if len(got) != 0 {
				t.Errorf("%s: failures %+v, want none", s.name, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Account != "home" || !strings.Contains(got[0].Reason, s.want) || !got[0].Since.Equal(start.Add(s.since)) {
			t.Errorf("%s: failures %+v, want home's, saying %q, since %s", s.name, got, s.want, start.Add(s.since))
		}
	}
}

// An ask the caller gave up on says nothing about the harness, so it neither
// records a failure nor clears one.
func TestModelsFailureIgnoresACallerThatStopped(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	modelsNow = func() time.Time { return now }
	t.Cleanup(func() { modelsNow = time.Now })
	var failWith error
	a := &asker{answer: func(string, string) ([]string, error) { return nil, failWith }}
	a.install(t)
	resetModels(t)
	build := func(ctx context.Context) []ModelsFailure {
		found := []harness.Detected{ready(t, "claude")}
		addModels(ctx, found, config.Default(), nil)
		return ModelsFailures()
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()

	failWith = errors.New("no answer")
	if got := build(stopped); len(got) != 0 {
		t.Errorf("failures %+v from a build nobody waited for", got)
	}
	if got := build(context.Background()); len(got) != 1 {
		t.Fatalf("failures %+v, want the one ask that failed", got)
	}
	now = now.Add(modelsRetry)
	failWith = nil
	if got := build(stopped); len(got) != 1 {
		t.Errorf("failures %+v, want the last one kept by a build nobody waited for", got)
	}
}
