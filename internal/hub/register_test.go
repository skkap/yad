package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

func registerBody(t *testing.T, caps v1.Capabilities) string {
	t.Helper()
	b, err := json.Marshal(v1.RegisterRequest{Capabilities: caps})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func headers(bearer string) map[string]string {
	h := map[string]string{v1.HeaderProtocol: v1.Version}
	if bearer != "" {
		h["Authorization"] = "Bearer " + bearer
	}
	return h
}

func TestRegisterBurnsTheToken(t *testing.T) {
	f := newFixture(t)
	tok := f.token(t, time.Hour)
	body := registerBody(t, doc("r1"))

	req := newRequest(t, "/v1/runners/register", body, headers(tok))
	rec := serve(f.hub, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	var res v1.RegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.RunnerCredential, runnerCredentialPrefix) || res.SyncIntervalMS != 15000 || res.LeaseMS != 60000 {
		t.Errorf("response = %+v", res)
	}
	r, err := f.store.GetRunner(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if r.CredentialHash != hashSecret(res.RunnerCredential) || strings.Contains(r.CredentialHash, res.RunnerCredential) {
		t.Error("the credential is not stored as its hash alone")
	}

	// The same token a second time: dead, with the way forward.
	_, env := post(t, f.hub, "/v1/runners/register", registerBody(t, doc("r2")), headers(tok))
	if env.Error.Code != v1.CodeUnauthorized || !strings.Contains(env.Error.Message, "already used") || !strings.Contains(env.Error.NextAction, "hub token create") {
		t.Errorf("reused token: %+v", env.Error)
	}
}

func TestRegisterRefusals(t *testing.T) {
	f := newFixture(t)
	expired := f.token(t, time.Minute)
	f.clock.Advance(2 * time.Minute)
	live := f.token(t, time.Hour)
	bad := doc("r/1")
	for _, tc := range []struct {
		name, bearer, body string
		status             int
		code, message      string
	}{
		{"no token", "", registerBody(t, doc("r1")), 401, v1.CodeUnauthorized, "registration token"},
		{"unknown token", "yadreg_nope", registerBody(t, doc("r1")), 401, v1.CodeUnauthorized, "never issued"},
		{"expired token", expired, registerBody(t, doc("r1")), 401, v1.CodeUnauthorized, "expired"},
		{"bad runner id", live, registerBody(t, bad), 400, v1.CodeInvalid, "runner_id"},
		{"dot-dot runner id", live, registerBody(t, doc("..")), 400, v1.CodeInvalid, "runner_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, env := post(t, f.hub, "/v1/runners/register", tc.body, headers(tc.bearer))
			if res.StatusCode != tc.status || env.Error.Code != tc.code || !strings.Contains(env.Error.Message, tc.message) || env.Error.NextAction == "" {
				t.Errorf("%d %+v", res.StatusCode, env.Error)
			}
		})
	}
	// A refused request must not have burned the live token.
	if res, env := post(t, f.hub, "/v1/runners/register", registerBody(t, doc("r1")), headers(live)); res.StatusCode != 200 {
		t.Errorf("live token after refusals: %d %+v", res.StatusCode, env.Error)
	}
}

// Runner ids are not secret, so a token for a new runner must not take over
// one that exists: that would hand its sessions and their grants to whoever
// holds any token. Only a token issued for that runner replaces its
// credential, and a refused request burns nothing.
func TestOnlyATokenForThatRunnerReRegistersIt(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "victim")
	f.register(t, "other")
	newRunner := f.token(t, time.Hour)
	res, env := post(t, f.hub, "/v1/runners/register", registerBody(t, doc("victim")), headers(newRunner))
	if res.StatusCode != http.StatusConflict || env.Error.Code != v1.CodeConflict || !strings.Contains(env.Error.NextAction, "--runner") || !strings.Contains(env.Error.NextAction, "--runner victim") {
		t.Fatalf("takeover with a new-runner token: %d %+v", res.StatusCode, env.Error)
	}
	if res, _ := f.sync(t, "victim", cred, v1.SyncRequest{RunnerID: "victim"}); res.StatusCode != http.StatusOK {
		t.Errorf("the victim's credential stopped working: %d", res.StatusCode)
	}
	forOther := f.tokenFor(t, time.Hour, "other")
	if res, env := post(t, f.hub, "/v1/runners/register", registerBody(t, doc("victim")), headers(forOther)); res.StatusCode != http.StatusUnauthorized || !strings.Contains(env.Error.Message, `"other"`) {
		t.Errorf("takeover with a token for another runner: %d %+v", res.StatusCode, env.Error)
	}
	// Both refused tokens still do what they were issued for.
	if res, env := post(t, f.hub, "/v1/runners/register", registerBody(t, doc("fresh")), headers(newRunner)); res.StatusCode != http.StatusOK {
		t.Errorf("new-runner token after its refusal: %d %+v", res.StatusCode, env.Error)
	}
	if res, env := post(t, f.hub, "/v1/runners/register", registerBody(t, doc("other")), headers(forOther)); res.StatusCode != http.StatusOK {
		t.Errorf("runner-bound token after its refusal: %d %+v", res.StatusCode, env.Error)
	}
	if _, _, err := IssueRegistrationToken(context.Background(), f.store, time.Hour, f.clock.Now(), "nobody"); err == nil {
		t.Error("issued a re-registration token for a runner the hub does not know")
	}
}

// Re-registering with a token issued for the runner replaces the credential:
// the recovery for a runner that lost its credential file. The old credential
// stops working.
func TestReRegisterRotatesTheCredential(t *testing.T) {
	f := newFixture(t)
	first := f.register(t, "r1")
	second := f.register(t, "r1")
	if first == second {
		t.Fatal("same credential twice")
	}
	if res, _ := f.sync(t, "r1", first, v1.SyncRequest{RunnerID: "r1"}); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("old credential answered %d", res.StatusCode)
	}
	if res, env := f.sync(t, "r1", second, v1.SyncRequest{RunnerID: "r1"}); res.StatusCode != http.StatusOK {
		t.Errorf("new credential answered %d %+v", res.StatusCode, env.Error)
	}
}

func TestTokenTTLIsBounded(t *testing.T) {
	f := newFixture(t)
	for _, ttl := range []time.Duration{0, -time.Second, MaxTokenTTL + time.Second} {
		if _, _, err := IssueRegistrationToken(context.Background(), f.store, ttl, f.clock.Now(), ""); err == nil {
			t.Errorf("ttl %s accepted", ttl)
		}
	}
	tok, exp, err := IssueRegistrationToken(context.Background(), f.store, DefaultTokenTTL, f.clock.Now(), "")
	if err != nil || !strings.HasPrefix(tok, registrationTokenPrefix) || !exp.Equal(f.clock.Now().Add(time.Hour)) {
		t.Errorf("%q %v %v", tok, exp, err)
	}
	row, err := f.store.GetRegistrationToken(context.Background(), hashSecret(tok))
	if err != nil || row.UsedAt.Valid {
		t.Errorf("stored token = %+v, %v", row, err)
	}
}

// A refused runner is told what to run on each machine, as commands it can
// paste: a token from this hub's database, run at the hub, and a connect run
// at the runner. The hub knows neither the runner's profile, nor the URL and
// connection name it reaches this hub by, nor — without Options.Command — its
// own profile and database, so each is a placeholder that says what it wants.
// A bare `yad connect` would be refused for want of a URL, and a bare `yad hub
// token create` issues into the default profile's database, whose token this
// hub then refuses as one it never issued.
func TestRegistrationAdviceRunsAsPrinted(t *testing.T) {
	f := newFixture(t)
	f.register(t, "victim")
	connect := []string{"yad", "--profile", "<runner profile>", "connect", "<hub url>", "--name", "<connection name>", "--token", "<new token>"}
	token := func(args ...string) []string {
		argv := append([]string{"yad", "--profile", "<hub profile>", "hub", "token", "create"}, args...)
		return append(argv, "--db", "<the database yad hub serve was given>")
	}
	spent := f.token(t, time.Hour)
	post(t, f.hub, "/v1/runners/register", registerBody(t, doc("r1")), headers(spent))
	for _, tc := range []struct {
		name  string
		send  func() v1.ErrorEnvelope
		token []string
	}{
		{"a spent token", func() v1.ErrorEnvelope {
			_, env := post(t, f.hub, "/v1/runners/register", registerBody(t, doc("r2")), headers(spent))
			return env
		}, token()},
		{"a new-runner token for a known runner", func() v1.ErrorEnvelope {
			_, env := post(t, f.hub, "/v1/runners/register", registerBody(t, doc("victim")), headers(f.token(t, time.Hour)))
			return env
		}, token("--runner", "victim")},
		{"a credential the hub does not hold", func() v1.ErrorEnvelope {
			_, env := f.sync(t, "victim", "yadcred_nope", v1.SyncRequest{RunnerID: "victim"})
			return env
		}, token()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := tc.send().Error.NextAction
			cmds := shellwordtest.Commands(next, "yad ")
			if len(cmds) != 2 {
				t.Fatalf("want a token command and a connect command in %q", next)
			}
			shellwordtest.Check(t, cmds[0], tc.token...)
			shellwordtest.Check(t, cmds[1], connect...)
		})
	}
}
