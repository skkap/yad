package hubapiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"
)

// A watch on a run that takes hours outlives a hub restart: a failed poll is
// asked again, while a refusal ends it at once instead of looping forever.
func TestFollowRetriesOnlyWhatMayPass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		first   int
		wantErr bool
		calls   int32
	}{
		{"hub briefly down", http.StatusServiceUnavailable, false, 2},
		{"proxy timed out", http.StatusRequestTimeout, false, 2},
		{"rate limited", http.StatusTooManyRequests, false, 2},
		{"token revoked", http.StatusUnauthorized, true, 1},
		{"no such run", http.StatusNotFound, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer yadadm_t" || r.URL.Path != hubapi.BasePath+"/runs/r1/events" {
					t.Errorf("request %s with %q", r.URL.Path, r.Header.Get("Authorization"))
				}
				w.Header().Set("Content-Type", "application/json")
				if calls.Add(1) == 1 {
					w.WriteHeader(tc.first)
					_ = json.NewEncoder(w).Encode(v1.ErrorEnvelope{Error: v1.Error{Code: "x", Message: "m", NextAction: "n"}})
					return
				}
				_ = json.NewEncoder(w).Encode(hubapi.EventPage{Done: true, NextAfter: 1, Events: []v1.Event{{Seq: 1, Kind: v1.EventText}},
					Run: hubapi.Run{RunID: "r1", State: hubapi.RunState(v1.RunSucceeded)}})
			}))
			defer srv.Close()
			c, err := New(srv.URL, "yadadm_t")
			if err != nil {
				t.Fatal(err)
			}
			var pages int
			end, err := c.Follow(context.Background(), "r1", 0, func(hubapi.EventPage) error { pages++; return nil })
			if (err != nil) != tc.wantErr || calls.Load() != tc.calls {
				t.Fatalf("err %v after %d calls", err, calls.Load())
			}
			if !tc.wantErr && (pages != 1 || end.State != hubapi.RunState(v1.RunSucceeded)) {
				t.Errorf("pages %d, end %+v", pages, end)
			}
		})
	}
}

// The admin token rides on every request, so it goes in cleartext only to
// this machine.
func TestNewRefusesCleartextToAnotherHost(t *testing.T) {
	if _, err := New("http://hub.example", "yadadm_t"); err == nil {
		t.Error("plain http to another host was accepted")
	}
	if _, err := New("http://127.0.0.1:7878", "yadadm_t"); err != nil {
		t.Error(err)
	}
}

// The runner's client's rule, for `yad hub run`: --hub and $YAD_HUB_URL may
// carry a credential in their userinfo, and an unreachable or redirecting hub
// is quoted back in the error that command prints.
func TestAnErrorNeverQuotesACredentialFromAURL(t *testing.T) {
	const secret = "sk-secret-token"
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+secret+"-2@"+r.Host+"/elsewhere", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	for _, tc := range []struct{ name, base string }{
		// Port 1: nothing listens, so the call fails at the transport.
		{"unreachable, token as username", "http://" + secret + "@127.0.0.1:1"},
		{"unreachable, token as password", "http://runner:" + secret + "@127.0.0.1:1"},
		{"redirected", "http://" + secret + "@" + strings.TrimPrefix(redirect.URL, "http://") + ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(tc.base, "a-token")
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Runners(context.Background())
			switch {
			case err == nil:
				t.Fatal("the call succeeded; it was meant to fail and quote a URL")
			case strings.Contains(err.Error(), secret):
				t.Errorf("the error carries the credential: %v", err)
			case !strings.Contains(err.Error(), "127.0.0.1:"):
				t.Errorf("the error no longer says which hub it could not reach: %v", err)
			}
		})
	}
}
