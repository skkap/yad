package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
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
	if env.Error.Code != v1.CodeUnauthorized || !strings.Contains(env.Error.Message, "already used") || !strings.Contains(env.Error.NextAction, "yad hub token create") {
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

// Registering an id the hub knows replaces the credential: the recovery for a
// runner that lost its credential file. The old credential stops working.
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
		if _, _, err := IssueRegistrationToken(context.Background(), f.store, ttl, f.clock.Now()); err == nil {
			t.Errorf("ttl %s accepted", ttl)
		}
	}
	tok, exp, err := IssueRegistrationToken(context.Background(), f.store, DefaultTokenTTL, f.clock.Now())
	if err != nil || !strings.HasPrefix(tok, registrationTokenPrefix) || !exp.Equal(f.clock.Now().Add(time.Hour)) {
		t.Errorf("%q %v %v", tok, exp, err)
	}
	row, err := f.store.GetRegistrationToken(context.Background(), hashSecret(tok))
	if err != nil || row.UsedAt.Valid {
		t.Errorf("stored token = %+v, %v", row, err)
	}
}
