package runner

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/hub"
	"github.com/skkap/yad/internal/hubapiclient"
)

// recordingHub keeps every sync request as it went over the wire.
type recordingHub struct {
	Hub
	mu     sync.Mutex
	bodies []string
}

func (r *recordingHub) Sync(ctx context.Context, id string, req v1.SyncRequest) (v1.SyncResponse, error) {
	b, _ := json.Marshal(req)
	r.mu.Lock()
	r.bodies = append(r.bodies, string(b))
	r.mu.Unlock()
	return r.Hub.Sync(ctx, id, req)
}

func (r *recordingHub) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.bodies, "\n")
}

// loginE2E is a runner syncing with yad hub in process, its Logins driving the
// fake claude, and the hub's service API as a hub's UI would call it.
type loginE2E struct {
	*loginRig
	loop   *Loop
	hub    *recordingHub
	api    *hubapiclient.Client
	runner string
}

func newLoginE2E(t *testing.T, mode string) *loginE2E {
	t.Helper()
	e := newEnv(t)
	r := newLoginRig(t, e, accountConfig("work"), mode)
	l := e.loop(t, 1)
	rec := &recordingHub{Hub: l.Hub}
	l.Hub, l.Logins, l.Log = rec, r.Logins, r.Logins.Log
	tok, err := hub.IssueAdminToken(context.Background(), e.hubStore, "ui", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	api, err := hubapiclient.New(strings.TrimSuffix(e.url, hub.BasePath), tok)
	if err != nil {
		t.Fatal(err)
	}
	mustSync(t, l)
	return &loginE2E{loginRig: r, loop: l, hub: rec, api: api, runner: l.RunnerID}
}

// syncUntil syncs until the hub's view of the login satisfies done.
func (x *loginE2E) syncUntil(t *testing.T, id string, done func(hubapi.Login) bool) hubapi.Login {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		mustSync(t, x.loop)
		l, err := x.api.Login(context.Background(), x.runner, id)
		if err != nil {
			t.Fatal(err)
		}
		if done(l) {
			return l
		}
		if time.Now().After(deadline) {
			t.Fatalf("login %s stopped at %+v\nlog:\n%s", id, l, x.log.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func is(s v1.LoginState) func(hubapi.Login) bool {
	return func(l hubapi.Login) bool { return l.State == hubapi.LoginState(s) }
}

// Nothing a login carries in may be left anywhere on the runner's side but
// where it belongs: not in the log, not in a sync the runner sent, not in
// state.db or any file under the profile's data but the account's own token
// file. Nor in hub.db once the store has let go of it.
func (x *loginE2E) secretsStayPut(t *testing.T, secrets ...string) {
	t.Helper()
	log, syncs := x.log.String(), x.hub.all()
	var files []string
	filepath.WalkDir(x.e.paths.Data, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && filepath.Base(path) != "yad-oauth-token" {
			files = append(files, path)
		}
		return nil
	})
	for _, s := range secrets {
		if strings.Contains(log, s) {
			t.Errorf("the runner's log holds a secret:\n%s", log)
		}
		if strings.Contains(syncs, s) {
			t.Errorf("a sync request carried a secret")
		}
		for _, f := range files {
			if b, err := os.ReadFile(f); err == nil && strings.Contains(string(b), s) {
				t.Errorf("%s holds a secret", f)
			}
		}
	}
}

// The link way end to end through yad hub: the start delivered at a sync,
// claude's own link on the hub, the owner's code back through it, and the
// account free because claude's own check says so — with the code nowhere it
// should not be.
func TestAHubLoginByLinkThroughYadHub(t *testing.T) {
	x := newLoginE2E(t, "")
	ctx := context.Background()
	l, err := x.api.StartLogin(ctx, x.runner, hubapi.LoginRequest{Harness: "claude", Account: "work"})
	if err != nil {
		t.Fatal(err)
	}
	l = x.syncUntil(t, l.LoginID, is(v1.LoginWaiting))
	if l.URL != fakeLoginURL {
		t.Fatalf("the hub shows %q, not claude's link", l.URL)
	}
	if _, err := x.api.SendLoginCode(ctx, x.runner, l.LoginID, fakeLoginCode); err != nil {
		t.Fatal(err)
	}
	x.syncUntil(t, l.LoginID, is(v1.LoginSucceeded))
	if got := accountState(t, x.e, "work"); got != v1.AccountFree {
		t.Errorf("the account is %q, want free", got)
	}
	// Reported until a sync carrying the end was answered, then forgotten.
	mustSync(t, x.loop)
	if got := x.Reports("hub"); len(got) != 0 {
		t.Errorf("the runner still reports %+v after the hub heard the end", got)
	}
	x.secretsStayPut(t, fakeLoginCode)
}

// The token way end to end: delivered once, stored as the account's login,
// blanked on the hub once the runner has reported the login, and in no sync,
// log or file of the runner's but the account's token file.
func TestAHubLoginByTokenThroughYadHub(t *testing.T) {
	x := newLoginE2E(t, "")
	ctx := context.Background()
	const tok = "sk-ant-oat01-pasted-into-the-hub"
	l, err := x.api.StartLogin(ctx, x.runner, hubapi.LoginRequest{Harness: "claude", Account: "work", Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	x.syncUntil(t, l.LoginID, is(v1.LoginSucceeded))
	row, err := x.e.hubStore.GetLogin(ctx, l.LoginID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Token != "" {
		t.Error("hub.db still holds the token after the runner reported the login")
	}
	if got := accountState(t, x.e, "work"); got != v1.AccountFree {
		t.Errorf("the account is %q, want free", got)
	}
	x.secretsStayPut(t, tok)
}

// What goes wrong, through yad hub: a wrong code fails, a login whose code
// never comes expires, a newer login supersedes an older one on the runner,
// and a cancel from the hub ends one.
func TestAHubLoginThatDoesNotTakeThroughYadHub(t *testing.T) {
	ctx := context.Background()
	t.Run("wrong code", func(t *testing.T) {
		x := newLoginE2E(t, "")
		l, err := x.api.StartLogin(ctx, x.runner, hubapi.LoginRequest{Harness: "claude", Account: "work"})
		if err != nil {
			t.Fatal(err)
		}
		x.syncUntil(t, l.LoginID, is(v1.LoginWaiting))
		if _, err := x.api.SendLoginCode(ctx, x.runner, l.LoginID, "a-wrong-code"); err != nil {
			t.Fatal(err)
		}
		l = x.syncUntil(t, l.LoginID, is(v1.LoginFailed))
		if !strings.Contains(l.Error, "start a new login") {
			t.Errorf("the hub shows %q", l.Error)
		}
		x.secretsStayPut(t, "a-wrong-code")
	})
	t.Run("expiry", func(t *testing.T) {
		x := newLoginE2E(t, "")
		x.CodeWait = 300 * time.Millisecond
		l, err := x.api.StartLogin(ctx, x.runner, hubapi.LoginRequest{Harness: "claude", Account: "work"})
		if err != nil {
			t.Fatal(err)
		}
		x.syncUntil(t, l.LoginID, is(v1.LoginExpired))
	})
	t.Run("supersede and cancel", func(t *testing.T) {
		x := newLoginE2E(t, "")
		first, err := x.api.StartLogin(ctx, x.runner, hubapi.LoginRequest{Harness: "claude", Account: "work"})
		if err != nil {
			t.Fatal(err)
		}
		x.syncUntil(t, first.LoginID, is(v1.LoginWaiting))
		second, err := x.api.StartLogin(ctx, x.runner, hubapi.LoginRequest{Harness: "claude", Account: "work"})
		if err != nil {
			t.Fatal(err)
		}
		x.syncUntil(t, first.LoginID, is(v1.LoginCancelled))
		x.syncUntil(t, second.LoginID, is(v1.LoginWaiting))
		if _, err := x.api.CancelLogin(ctx, x.runner, second.LoginID); err != nil {
			t.Fatal(err)
		}
		l := x.syncUntil(t, second.LoginID, is(v1.LoginCancelled))
		if l.CancelRequestedAt != nil {
			t.Errorf("a cancelled login still shows a cancel asked for: %+v", l)
		}
	})
}

// The sequence a hub's own guess used to break, through yad hub: a token
// delivered, stored and taken on the runner, and a link login for the same
// account started before the runner's next sync. The hub hears that the first
// took rather than calling it replaced before it arrived, the second goes out
// after it, and when the second fails the account keeps running on the token
// the first stored — both sides telling the same story.
func TestALoginReplacedAfterItWasSentEndsAsTheRunnerSays(t *testing.T) {
	x := newLoginE2E(t, "")
	ctx := context.Background()
	const tok = "sk-ant-oat01-stored-before-the-hub-heard"
	a, err := x.api.StartLogin(ctx, x.runner, hubapi.LoginRequest{Harness: "claude", Account: "work", Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	mustSync(t, x.loop) // the answer carries login_token for a
	x.until(t, a.LoginID, v1.LoginSucceeded)
	b, err := x.api.StartLogin(ctx, x.runner, hubapi.LoginRequest{Harness: "claude", Account: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if l, err := x.api.Login(ctx, x.runner, a.LoginID); err != nil || l.State.Terminal() {
		t.Errorf("before the runner's next sync the hub already says a ended: %+v %v", l, err)
	}
	if got := x.syncUntil(t, a.LoginID, func(l hubapi.Login) bool { return l.State.Terminal() }); got.State != hubapi.LoginState(v1.LoginSucceeded) {
		t.Fatalf("the hub says a ended %s (%s); the runner stored its token and it took", got.State, got.Error)
	}
	x.syncUntil(t, b.LoginID, is(v1.LoginWaiting))
	if _, err := x.api.SendLoginCode(ctx, x.runner, b.LoginID, "a-wrong-code"); err != nil {
		t.Fatal(err)
	}
	x.syncUntil(t, b.LoginID, is(v1.LoginFailed))
	home := filepath.Join(x.e.paths.Data, "accounts", "claude", "work")
	if env, _ := account.TurnEnv("claude", home); !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN="+tok) {
		t.Error("the account no longer runs on the token the hub says took")
	}
	if got := accountState(t, x.e, "work"); got != v1.AccountFree {
		t.Errorf("the account is %q, want free", got)
	}
}

// An account added and removed from yad hub, end to end (decision 0057): the
// add delivered with add and listed once its login took, the account then in
// the health the hub reads; the removal repeated until that health leaves it
// out, and then no longer sent. From a connection whose owner has not let it,
// the hub is never told it may, and refuses both.
func TestAnAccountAddedAndRemovedThroughYadHub(t *testing.T) {
	x := newLoginE2E(t, "")
	x.loop.Accounts, x.loop.Paths, x.loop.ManageAccounts = x.Accounts, x.e.paths, true
	// The document names each harness's accounts, as capability.Build's
	// does: a hub reads a removal's end from it as well as from health.
	fixed := x.loop.Capabilities
	x.loop.Capabilities = func() v1.Capabilities {
		d := fixed()
		d.Harnesses = slices.Clone(d.Harnesses)
		for i, h := range d.Harnesses {
			for _, l := range x.Accounts.Lists()[h.ID] {
				d.Harnesses[i].Accounts = append(d.Harnesses[i].Accounts, v1.AccountReport{Label: l, State: v1.AccountFree})
			}
		}
		return d
	}
	ctx := context.Background()
	mustSync(t, x.loop) // the document with accounts in it

	l, err := x.api.StartLogin(ctx, x.runner, hubapi.LoginRequest{Harness: "claude", Account: "second", Add: true})
	if err != nil {
		t.Fatal(err)
	}
	x.syncUntil(t, l.LoginID, is(v1.LoginWaiting))
	if _, err := x.api.SendLoginCode(ctx, x.runner, l.LoginID, fakeLoginCode); err != nil {
		t.Fatal(err)
	}
	x.syncUntil(t, l.LoginID, is(v1.LoginSucceeded))
	mustSync(t, x.loop)
	a, err := x.api.Account(ctx, x.runner, "claude", "second")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Listed || a.State != v1.AccountFree {
		t.Fatalf("the hub reads the added account as %+v, want listed and free", a)
	}

	if a, err = x.api.RemoveAccount(ctx, x.runner, "claude", "second"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for a.RemoveRequestedAt != nil || a.Listed {
		if time.Now().After(deadline) {
			t.Fatalf("the removal stopped at %+v\nlog:\n%s", a, x.log.String())
		}
		mustSync(t, x.loop)
		if a, err = x.api.Account(ctx, x.runner, "claude", "second"); err != nil {
			t.Fatal(err)
		}
	}
	if x.Accounts.Lists().Has(account.Ref{Harness: "claude", Label: "second"}) {
		t.Error("the runner still lists the removed account")
	}
	if _, err := os.Stat(account.HomeDir(x.e.paths.Data, "claude", "second")); !os.IsNotExist(err) {
		t.Errorf("the removed account's home is still there (%v)", err)
	}
	for _, c := range mustSync(t, x.loop).Controls {
		if c.Kind == v1.ControlRemoveAccount {
			t.Errorf("the hub still sends %+v once health left the account out", c)
		}
	}

	// Turned off for this hub: the document it is sent says so, and it
	// refuses both rather than sending what the runner would ignore.
	x.loop.ManageAccounts = false
	mustSync(t, x.loop)
	if _, err := x.api.StartLogin(ctx, x.runner, hubapi.LoginRequest{Harness: "claude", Account: "third", Add: true}); err == nil || !strings.Contains(err.Error(), "manage_accounts = false") {
		t.Errorf("an add to a runner that turned it off: %v", err)
	}
	if _, err := x.api.RemoveAccount(ctx, x.runner, "claude", "work"); err == nil {
		t.Error("a removal from a runner that turned it off was taken")
	}
}
