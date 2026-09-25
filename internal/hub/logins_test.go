package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"
)

func loginControls(res v1.SyncResponse) []v1.Control {
	var out []v1.Control
	for _, c := range res.Controls {
		switch c.Kind {
		case v1.ControlStartLogin, v1.ControlLoginCode, v1.ControlLoginToken, v1.ControlCancelLogin:
			out = append(out, c)
		}
	}
	return out
}

func withLogins(r v1.SyncRequest, logins ...v1.LoginReport) v1.SyncRequest {
	r.Logins = logins
	return r
}

func report(id string, state v1.LoginState, url string) v1.LoginReport {
	return v1.LoginReport{LoginID: id, Harness: "claude", Account: "work", Method: v1.LoginByLink, State: state, URL: url, UpdatedAt: time.Now()}
}

func (f *fixture) login(t *testing.T, tok, id string) hubapi.Login {
	t.Helper()
	var v hubapi.Login
	if code, e := f.api(t, "GET", "/runners/r1/logins/"+id, tok, nil, &v); code != http.StatusOK {
		t.Fatalf("get %s: %d %+v", id, code, e)
	}
	return v
}

// raw is a service API answer's body as it went over the wire, for a test
// that proves what it does not carry.
func (f *fixture) raw(t *testing.T, method, path, token string) string {
	t.Helper()
	req := httptest.NewRequest(method, hubapi.BasePath+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := serve(f.hub, req)
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func (f *fixture) loginRow(t *testing.T, id string) (code, token string) {
	t.Helper()
	l, err := f.store.GetLogin(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return l.Code, l.Token
}

// A link login through yad hub: start_login repeated until the runner reports
// the login, the link shown once it is waiting, the code refused before then
// and delivered after — repeated while the runner still says waiting, and
// gone from the database once it moves on — and the runner's end standing.
func TestAHubLoginByLink(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	old := f.register(t, "old")
	f.mustSync(t, "old", old, stale("old", 1))
	cred := f.register(t, "r1")
	f.mustSync(t, "r1", cred, first("r1", 1))

	start := hubapi.LoginRequest{LoginID: "lg1", Harness: "claude", Account: "work"}
	for _, c := range []struct {
		path string
		code int
	}{
		{"/runners/nope/logins", http.StatusNotFound},
		{"/runners/old/logins", http.StatusConflict},
	} {
		if code, e := f.api(t, "POST", c.path, tok, start, nil); code != c.code {
			t.Errorf("%s: %d %+v, want %d", c.path, code, e, c.code)
		}
	}
	var view hubapi.Login
	if code, e := f.api(t, "POST", "/runners/r1/logins", tok, start, &view); code != http.StatusCreated || view.State != hubapi.LoginRequested || view.Method != v1.LoginByLink {
		t.Fatalf("start: %d %+v %+v", code, e, view)
	}
	if code, _ := f.api(t, "POST", "/runners/r1/logins", tok, start, nil); code != http.StatusConflict {
		t.Errorf("the same login id twice: %d, want 409", code)
	}

	// Repeated until reported: nothing acknowledges a control.
	for range 2 {
		cs := loginControls(f.mustSync(t, "r1", cred, req("r1", 1)))
		if len(cs) != 1 || cs[0].Kind != v1.ControlStartLogin || cs[0].LoginID != "lg1" || cs[0].Harness != "claude" || cs[0].Account != "work" {
			t.Fatalf("controls %+v, want start_login for lg1", cs)
		}
	}
	if cs := loginControls(f.mustSync(t, "r1", cred, withLogins(req("r1", 1), report("lg1", v1.LoginStarting, "")))); len(cs) != 0 {
		t.Fatalf("once reported starting: %+v", cs)
	}
	code := "the-owners-code"
	if c, _ := f.api(t, "POST", "/runners/r1/logins/lg1/code", tok, hubapi.LoginCodeRequest{Code: code}, nil); c != http.StatusConflict {
		t.Errorf("a code before the link is out: %d, want 409", c)
	}
	const link = "https://claude.com/cai/oauth/authorize?code=true&state=x"
	f.mustSync(t, "r1", cred, withLogins(req("r1", 1), report("lg1", v1.LoginWaiting, link)))
	f.api(t, "GET", "/runners/r1/logins/lg1", tok, nil, &view)
	if view.State != hubapi.LoginState(v1.LoginWaiting) || view.URL != link {
		t.Fatalf("waiting: %+v", view)
	}
	if c, e := f.api(t, "POST", "/runners/r1/logins/lg1/code", tok, hubapi.LoginCodeRequest{Code: code}, &view); c != http.StatusOK || !view.CodeSent {
		t.Fatalf("code: %d %+v %+v", c, e, view)
	}
	if body := f.raw(t, "GET", "/runners/r1/logins/lg1", tok); strings.Contains(body, code) {
		t.Errorf("the service API returned the code: %s", body)
	}
	for range 2 {
		cs := loginControls(f.mustSync(t, "r1", cred, withLogins(req("r1", 1), report("lg1", v1.LoginWaiting, link))))
		if len(cs) != 1 || cs[0].Kind != v1.ControlLoginCode || cs[0].Code != code {
			t.Fatalf("controls %+v, want login_code with the code", cs)
		}
	}
	if cs := loginControls(f.mustSync(t, "r1", cred, withLogins(req("r1", 1), report("lg1", v1.LoginChecking, "")))); len(cs) != 0 {
		t.Fatalf("once checking: %+v", cs)
	}
	if c, _ := f.loginRow(t, "lg1"); c != "" {
		t.Errorf("the code is still in hub.db once the runner took it")
	}
	f.mustSync(t, "r1", cred, withLogins(req("r1", 1), report("lg1", v1.LoginSucceeded, "")))
	// The end stands: repeated, or left out once answered.
	f.mustSync(t, "r1", cred, withLogins(req("r1", 1), report("lg1", v1.LoginFailed, "")))
	f.mustSync(t, "r1", cred, req("r1", 1))
	f.api(t, "GET", "/runners/r1/logins/lg1", tok, nil, &view)
	if view.State != hubapi.LoginState(v1.LoginSucceeded) {
		t.Errorf("after the end was reported: %+v", view)
	}
	if c, _ := f.api(t, "POST", "/runners/r1/logins/lg1/code", tok, hubapi.LoginCodeRequest{Code: code}, nil); c != http.StatusConflict {
		t.Errorf("a code for an ended login: %d, want 409", c)
	}
}

// A token login: the token delivered in login_token until the runner reports
// the login, and blanked in hub.db the moment it does; never in an answer of
// the service API; refused before it is stored when it names no account or
// is not one token.
func TestAHubLoginByToken(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	f.mustSync(t, "r1", cred, first("r1", 1))
	const secret = "sk-ant-oat01-a-token-worth-a-year"

	for _, bad := range []hubapi.LoginRequest{
		{Harness: "claude", Token: secret},
		{Harness: "claude", Account: "work", Token: "two words"},
		{Harness: "claude", Account: "Not A Label"},
	} {
		code, e := f.api(t, "POST", "/runners/r1/logins", tok, bad, nil)
		if code != http.StatusBadRequest {
			t.Errorf("%+v: %d %+v, want 400", bad.Account, code, e)
		}
		if strings.Contains(e.Message+e.NextAction, secret) || strings.Contains(e.Message+e.NextAction, "two words") {
			t.Errorf("the refusal repeats the token: %+v", e)
		}
	}

	b, err := json.Marshal(hubapi.LoginRequest{LoginID: "lg1", Harness: "claude", Account: "work", Token: secret})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", hubapi.BasePath+"/runners/r1/logins", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	rec := serve(f.hub, r)
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	cs := loginControls(f.mustSync(t, "r1", cred, req("r1", 1)))
	if len(cs) != 1 || cs[0].Kind != v1.ControlLoginToken || cs[0].Token != secret || cs[0].Account != "work" {
		t.Fatalf("controls %+v, want login_token carrying the token", cs)
	}
	checking := report("lg1", v1.LoginChecking, "")
	checking.Method = v1.LoginByToken
	if cs := loginControls(f.mustSync(t, "r1", cred, withLogins(req("r1", 1), checking))); len(cs) != 0 {
		t.Fatalf("once the runner reported the login: %+v", cs)
	}
	if _, stored := f.loginRow(t, "lg1"); stored != "" {
		t.Error("the token is still in hub.db after the runner reported the login")
	}
	if body := f.raw(t, "GET", "/runners/r1/logins/lg1", tok); strings.Contains(body, secret) {
		t.Errorf("the service API returned the token: %s", body)
	}
}

// What time and silence decide: a login that never reaches its runner expires
// and loses its token; one its runner reported and then leaves out ended when
// the runner restarted; a newer login for the same account replaces one the
// runner has not heard of; and a cancel goes instead of the start, answered
// by the runner whether or not it had the login.
func TestAHubEndsLoginsItsRunnerCannotFinish(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	f.mustSync(t, "r1", cred, first("r1", 1))
	start := func(id, label, token string) {
		t.Helper()
		if code, e := f.api(t, "POST", "/runners/r1/logins", tok, hubapi.LoginRequest{LoginID: id, Harness: "claude", Account: label, Token: token}, nil); code != http.StatusCreated {
			t.Fatalf("start %s: %d %+v", id, code, e)
		}
	}
	state := func(id string) hubapi.Login {
		t.Helper()
		var view hubapi.Login
		if code, e := f.api(t, "GET", "/runners/r1/logins/"+id, tok, nil, &view); code != http.StatusOK {
			t.Fatalf("get %s: %d %+v", id, code, e)
		}
		return view
	}

	start("undelivered", "a", "sk-ant-oat01-never-sent")
	f.clock.Advance(loginDeliverWithin + time.Minute)
	if err := f.hub.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := state("undelivered"); v.State != hubapi.LoginState(v1.LoginExpired) {
		t.Errorf("a login no sync took: %+v", v)
	}
	if _, stored := f.loginRow(t, "undelivered"); stored != "" {
		t.Error("an expired login kept its token")
	}

	start("forgotten", "b", "")
	f.mustSync(t, "r1", cred, withLogins(req("r1", 1), report("forgotten", v1.LoginWaiting, "https://x.example/oauth/authorize")))
	f.mustSync(t, "r1", cred, req("r1", 1))
	if v := state("forgotten"); v.State != hubapi.LoginState(v1.LoginFailed) || !strings.Contains(v.Error, "restarted") {
		t.Errorf("a login the runner stopped reporting: %+v", v)
	}

	start("older", "c", "")
	start("newer", "c", "")
	if v := state("older"); v.State != hubapi.LoginState(v1.LoginCancelled) || !strings.Contains(v.Error, "newer") {
		t.Errorf("a login replaced before it reached the runner: %+v", v)
	}
	var view hubapi.Login
	// Never sent: a cancel ends it here, and nothing goes to the runner.
	if code, e := f.api(t, "POST", "/runners/r1/logins/newer/cancel", tok, nil, &view); code != http.StatusOK || view.State != hubapi.LoginState(v1.LoginCancelled) {
		t.Fatalf("cancel before it was sent: %d %+v %+v", code, e, view)
	}
	start("sent", "d", "")
	if cs := loginControls(f.mustSync(t, "r1", cred, req("r1", 1))); len(cs) != 1 || cs[0].Kind != v1.ControlStartLogin {
		t.Fatalf("controls %+v, want start_login for sent", cs)
	}
	// Sent, and not reported: the runner may have it, so the cancel goes to
	// the runner and the login stays open until it answers.
	if code, e := f.api(t, "POST", "/runners/r1/logins/sent/cancel", tok, nil, &view); code != http.StatusOK || view.CancelRequestedAt == nil || view.State != hubapi.LoginRequested {
		t.Fatalf("cancel once sent: %d %+v %+v", code, e, view)
	}
	cs := loginControls(f.mustSync(t, "r1", cred, req("r1", 1)))
	if len(cs) != 1 || cs[0].Kind != v1.ControlCancelLogin || cs[0].LoginID != "sent" {
		t.Fatalf("controls %+v, want cancel_login for sent and nothing else", cs)
	}
	echo := v1.LoginReport{LoginID: "sent", State: v1.LoginCancelled, Error: "cancelled before this runner had it", UpdatedAt: time.Now()}
	f.mustSync(t, "r1", cred, withLogins(req("r1", 1), echo))
	if v := state("sent"); v.State != hubapi.LoginState(v1.LoginCancelled) || v.CancelRequestedAt != nil {
		t.Errorf("after the runner answered the cancel: %+v", v)
	}
	if cs := loginControls(f.mustSync(t, "r1", cred, req("r1", 1))); len(cs) != 0 {
		t.Errorf("controls after every login ended: %+v", cs)
	}
}

// 'requested' says only that no report has come: once an answer has carried a
// login, the runner may already have stored its token or taken it, and the
// hub ends it on its own word no more. Each way the hub used to — a newer
// login replacing it, the ten minutes a login may wait to be delivered, the
// half hour it may take — is tried against a login the runner did take, and
// each ends agreeing with the runner. An end the hub did write gives way to
// the runner's report of one.
func TestALoginSentToTheRunnerIsEndedByTheRunner(t *testing.T) {
	type step func(t *testing.T, f *fixture, tok, cred string)
	tokenLogin := func(id string) hubapi.LoginRequest {
		return hubapi.LoginRequest{LoginID: id, Harness: "claude", Account: "work", Token: "sk-ant-oat01-" + id}
	}
	succeeded := v1.LoginReport{LoginID: "a", Harness: "claude", Account: "work", Method: v1.LoginByToken, State: v1.LoginSucceeded, UpdatedAt: time.Now()}
	for _, tc := range []struct {
		name string
		// between runs after the answer carrying a's login_token, before the
		// sync reporting a's end.
		between step
		// then is what the sync reporting a's end must answer.
		then []v1.ControlKind
	}{
		{"a newer login for the account", func(t *testing.T, f *fixture, tok, _ string) {
			if code, e := f.api(t, "POST", "/runners/r1/logins", tok, hubapi.LoginRequest{LoginID: "b", Harness: "claude", Account: "work"}, nil); code != http.StatusCreated {
				t.Fatalf("start b: %d %+v", code, e)
			}
			// Not ended on the hub's word: asked to stop, and still open.
			if v := f.login(t, tok, "a"); v.State != hubapi.LoginRequested || v.CancelRequestedAt == nil {
				t.Errorf("a login already sent, once replaced: %+v", v)
			}
		}, []v1.ControlKind{v1.ControlStartLogin}},
		{"the delivery deadline", func(t *testing.T, f *fixture, tok, _ string) {
			f.clock.Advance(loginDeliverWithin)
			if err := f.hub.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			if v := f.login(t, tok, "a"); v.State != hubapi.LoginRequested {
				t.Errorf("a login already sent, past the delivery deadline: %+v", v)
			}
		}, nil},
		{"the finishing deadline, then the runner's word", func(t *testing.T, f *fixture, tok, _ string) {
			f.clock.Advance(loginFinishWithin + time.Minute)
			if err := f.hub.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			var v hubapi.Login
			f.api(t, "GET", "/runners/r1/logins/a", tok, nil, &v)
			if v.State != hubapi.LoginState(v1.LoginFailed) {
				t.Fatalf("a login sent and silent past the half hour: %+v", v)
			}
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tok := f.admin(t, "cli")
			cred := f.register(t, "r1")
			f.mustSync(t, "r1", cred, first("r1", 1))
			if code, e := f.api(t, "POST", "/runners/r1/logins", tok, tokenLogin("a"), nil); code != http.StatusCreated {
				t.Fatalf("start a: %d %+v", code, e)
			}
			// Just inside the ten minutes: the last answer before the mark.
			f.clock.Advance(loginDeliverWithin - time.Minute)
			if cs := loginControls(f.mustSync(t, "r1", cred, req("r1", 1))); len(cs) != 1 || cs[0].Kind != v1.ControlLoginToken {
				t.Fatalf("controls %+v, want login_token for a", cs)
			}
			// The runner stores the token and a takes; the hub hears at the
			// next sync.
			tc.between(t, f, tok, cred)
			var kinds []v1.ControlKind
			for _, c := range loginControls(f.mustSync(t, "r1", cred, withLogins(req("r1", 1), succeeded))) {
				kinds = append(kinds, c.Kind)
			}
			if !slices.Equal(kinds, tc.then) {
				t.Errorf("the sync reporting a's end answered %v, want %v", kinds, tc.then)
			}
			var v hubapi.Login
			f.api(t, "GET", "/runners/r1/logins/a", tok, nil, &v)
			if v.State != hubapi.LoginState(v1.LoginSucceeded) || v.Error != "" {
				t.Errorf("the hub says a is %+v; the runner stored its token and it took", v)
			}
		})
	}
}

// A runner whose document stops advertising login hears no login control,
// whatever is queued for it: it would ignore it, and the owner would wait on
// a login nobody is running.
func TestLoginControlsAreHeldBackFromARunnerThatStopsAdvertisingThem(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	f.mustSync(t, "r1", cred, first("r1", 1))
	if code, e := f.api(t, "POST", "/runners/r1/logins", tok, hubapi.LoginRequest{LoginID: "lg1", Harness: "claude"}, nil); code != http.StatusCreated {
		t.Fatalf("start: %d %+v", code, e)
	}
	downgraded := stale("r1", 1)
	downgraded.Fingerprint = "fp-r1-old"
	if cs := loginControls(f.mustSync(t, "r1", cred, downgraded)); len(cs) != 0 {
		t.Errorf("sent to a runner not advertising login: %+v", cs)
	}
}
