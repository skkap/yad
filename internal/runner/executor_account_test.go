package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/store/db"
)

// sentinel is the "credential" planted in an account home. It is shaped like
// the real thing so that anything copying a home's contents anywhere would
// carry it, and distinctive so a single strings.Contains finds it.
const sentinel = "sk-ant-oat01-A-CREDENTIAL-THAT-MUST-NEVER-LEAVE-THE-HOME"

// withAccounts is the env's executor with the owner's accounts configured.
func (e *env) accountExecutor(t *testing.T, cfg config.Config, adapters ...adapter.Adapter) (*Exec, *bytes.Buffer) {
	t.Helper()
	var logged bytes.Buffer
	x := e.executor(adapters...)
	x.Config = cfg
	// slog's JSON handler is what the daemon writes, so anything a log line
	// carries — a value, an error string, an attribute — lands in this buffer
	// exactly as it would land in yad.log.
	x.Log = slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return x, &logged
}

func accountConfig(labels ...string) config.Config {
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: labels}}
	return cfg
}

// plantCredential makes an account home and puts something credential-shaped
// in it, the way a real login would.
func plantCredential(t *testing.T, data, label string) string {
	t.Helper()
	home, err := account.Ensure(data, "claude", label)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q}}`, sentinel)
	if err := os.WriteFile(filepath.Join(home, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func spooledEvents(t *testing.T, e *env, id string) []v1.Event {
	t.Helper()
	rows, err := e.store.UnackedEvents(context.Background(), db.UnackedEventsParams{Connection: "hub", RunID: id, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]v1.Event, 0, len(rows))
	for _, r := range rows {
		var ev v1.Event
		if err := json.Unmarshal([]byte(r.Body), &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// A run takes the first account that can run a turn, and the run's events name
// it. The label is how an owner tells two subscriptions' work apart.
func TestRunUsesTheFirstFreeAccountAndNamesItInItsEvents(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, l := range []string{"personal", "work"} {
		plantCredential(t, e.paths.Data, l)
	}
	// The owner's first account cannot run a turn; the second can.
	if err := account.SetState(ctx, e.store.Queries, "claude", "personal", v1.AccountNeedsLogin, time.Now()); err != nil {
		t.Fatal(err)
	}
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("personal", "work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "done", NativeSessionID: "native-1"},
	}))
	claimAndRun(t, l, x)

	if got := localRun(t, e, "a"); got.State != "succeeded" {
		t.Fatalf("local state = %s", got.State)
	}
	var named string
	for _, ev := range spooledEvents(t, e, "a") {
		if ev.Kind == v1.EventStatus && ev.Status == "account" {
			named = ev.Text
		}
	}
	if named != "work" {
		t.Errorf("the run's events name account %q, want work — personal needs login", named)
	}
	if got := localRun(t, e, "a"); got.Account.String != "work" {
		t.Errorf("the run row records account %q, want work", got.Account.String)
	}
}

// The acceptance criterion, proved rather than intended: no credential or
// token from an account home reaches an event, a log line or the capability
// document.
func TestNoCredentialFromAnAccountHomeEverLeaves(t *testing.T) {
	e := newEnv(t)
	home := plantCredential(t, e.paths.Data, "work")
	cfg := accountConfig("work")

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, logged := e.accountExecutor(t, cfg, fakeHarness(fake.Script{
		Events: []v1.Event{{Kind: v1.EventText, Text: "working"}},
		// A harness that fails is the interesting case: a failure is where
		// diagnostics get helpful and start quoting things.
		Outcome: adapter.Outcome{State: v1.RunFailed, Error: &v1.RunError{Class: adapter.ClassHarness, Message: "something went wrong"}},
	}))
	claimAndRun(t, l, x)

	// The planted credential is really there, or the rest proves nothing.
	planted, err := os.ReadFile(filepath.Join(home, ".credentials.json"))
	if err != nil || !strings.Contains(string(planted), sentinel) {
		t.Fatalf("the credential was not planted: %v", err)
	}

	events, err := json.Marshal(spooledEvents(t, e, "a"))
	if err != nil {
		t.Fatal(err)
	}
	res, _ := outboxResult(t, e, "a")
	result, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	found := []harness.Detected{{Harness: harness.Catalog()[0], Present: true, Path: "/x/claude", Version: "2.1"}}
	accounts, err := account.Load(context.Background(), e.store.Queries, e.paths.Data, cfg)
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(capability.Harnesses(found, cfg, accounts))
	if err != nil {
		t.Fatal(err)
	}

	for _, place := range []struct{ what, body string }{
		{"the run's events", string(events)},
		{"the run's result", string(result)},
		{"the daemon's log", logged.String()},
		{"the capability document", string(document)},
	} {
		if strings.Contains(place.body, sentinel) {
			t.Errorf("a credential from an account home reached %s:\n%s", place.what, place.body)
		}
		// Not the token alone: the file it lives in must not be quoted either.
		if strings.Contains(place.body, ".credentials.json") {
			t.Errorf("%s names the credential file: %s", place.what, place.body)
		}
	}
	// The label is not a secret, and is the one thing that does travel.
	if !strings.Contains(string(events), `"work"`) {
		t.Errorf("the account label is missing from the events: %s", events)
	}
	if !strings.Contains(string(document), `"label":"work"`) {
		t.Errorf("the account label is missing from the document: %s", document)
	}
}

// Every account being unusable fails the run rather than quietly falling back
// to whatever login sits in the harness's default home.
func TestNoUsableAccountFailsTheRunAndSaysWhatToDo(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	if err := account.SetState(ctx, e.store.Queries, "claude", "work", v1.AccountNeedsLogin, time.Now()); err != nil {
		t.Fatal(err)
	}
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunSucceeded},
	}))
	claimAndRun(t, l, x)

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunFailed {
		t.Fatalf("result = %+v, %v", res, ok)
	}
	if res.Error.Class != hubClass(ClassRefused, false) {
		t.Errorf("error class = %q, want the class for a run the runner will not take", res.Error.Class)
	}
	if !strings.Contains(res.Error.Message, "yad account add claude work") {
		t.Errorf("the failure does not name the next action: %q", res.Error.Message)
	}
}

// A harness the owner gave no accounts runs on its own default home, and no
// home variable is put in its environment. This is every installation that
// existed before accounts did.
func TestAHarnessWithNoAccountsRunsOnItsOwnHome(t *testing.T) {
	e := newEnv(t)
	var seen adapter.Spec
	ad := &fake.Adapter{ID: "claude", Next: func(s adapter.Spec) fake.Script {
		seen = s
		return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}}
	}}
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, config.Default(), ad)
	claimAndRun(t, l, x)

	if got := localRun(t, e, "a"); got.State != "succeeded" {
		t.Fatalf("state = %s", got.State)
	}
	if seen.Home != "" {
		t.Errorf("a run with no accounts was given the home %q", seen.Home)
	}
	for _, v := range seen.Env {
		if strings.HasPrefix(v, "CLAUDE_CONFIG_DIR=") {
			t.Errorf("a run with no accounts had its home overridden: %q", v)
		}
	}
	for _, ev := range spooledEvents(t, e, "a") {
		if ev.Kind == v1.EventStatus && ev.Status == "account" {
			t.Errorf("a run with no accounts named an account: %q", ev.Text)
		}
	}
}

// The account's home reaches the harness, as the variable that harness reads.
func TestTheRunsHarnessIsPointedAtTheAccountsHome(t *testing.T) {
	e := newEnv(t)
	home := plantCredential(t, e.paths.Data, "work")
	var seen adapter.Spec
	ad := &fake.Adapter{ID: "claude", Next: func(s adapter.Spec) fake.Script {
		seen = s
		return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}}
	}}
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), ad)
	claimAndRun(t, l, x)

	if seen.Home != home {
		t.Errorf("spec home = %q, want %q", seen.Home, home)
	}
	if !slices.Contains(seen.Env, "CLAUDE_CONFIG_DIR="+home) {
		t.Errorf("the harness's environment is %v, without CLAUDE_CONFIG_DIR=%s", seen.Env, home)
	}
}
