package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub"
	hubstore "github.com/skkap/yad/internal/hub/store"
)

func TestSendsProtocolHeadersAndDecodes(t *testing.T) {
	var seen *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		var req v1.SyncRequest
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(v1.SyncResponse{NextSyncMS: 15000, Controls: []v1.Control{{Kind: v1.ControlDrain}}})
	}))
	defer srv.Close()

	c, err := New(srv.URL+"/v1/", "cred")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Sync(context.Background(), "runner/1", v1.SyncRequest{RunnerID: "runner/1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.NextSyncMS != 15000 || got.Controls[0].Kind != v1.ControlDrain {
		t.Errorf("decoded %+v", got)
	}
	if seen.URL.EscapedPath() != "/v1/runners/runner%2F1/sync" {
		t.Errorf("path = %s", seen.URL.EscapedPath())
	}
	for h, want := range map[string]string{"Authorization": "Bearer cred", v1.HeaderProtocol: "1", "Content-Type": "application/json"} {
		if seen.Header.Get(h) != want {
			t.Errorf("%s = %q, want %q", h, seen.Header.Get(h), want)
		}
	}
}

// The registration token authenticates exactly one request.
func TestRegisterUsesTheRegistrationToken(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(v1.RegisterResponse{RunnerCredential: "new-cred"})
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "")
	res, err := c.Register(context.Background(), "reg-token", v1.RegisterRequest{})
	if err != nil || res.RunnerCredential != "new-cred" || auth != "Bearer reg-token" {
		t.Errorf("res %+v err %v auth %q", res, err, auth)
	}
}

// Against the real hub, the envelope decodes into a code a runner can act
// on — and the credential appears nowhere in the error text.
func TestDecodesHubErrors(t *testing.T) {
	ctx := context.Background()
	hs, err := hubstore.Open(ctx, filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer hs.Close()
	srv := httptest.NewServer(hub.New(hub.Options{Store: hs}))
	defer srv.Close()
	c, _ := New(srv.URL+hub.BasePath, "super-secret-credential")
	// A credential this hub never issued: an error with the protocol's shape,
	// carrying the way out, and naming no secret.
	err = c.Deregister(ctx, "r1", "retiring")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnauthorized || Code(err) != v1.CodeUnauthorized {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "yad hub token create") {
		t.Errorf("the next action is lost: %v", err)
	}
	if strings.Contains(err.Error(), "super-secret-credential") {
		t.Error("the error text contains the credential")
	}
}

func TestNonProtocolErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<html>bad gateway</html>", http.StatusBadGateway)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "x")
	err := c.Result(context.Background(), "r", v1.Result{State: v1.RunSucceeded})
	if Code(err) != "" || !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %v", err)
	}
}

func TestRejectsBadURLs(t *testing.T) {
	for _, u := range []string{"", "hub.example/v1", "ftp://x/v1", "/v1"} {
		if _, err := New(u, ""); err == nil {
			t.Errorf("New(%q) accepted", u)
		}
	}
}

// Every call carries a bearer secret, so the client refuses a cleartext hop to
// another host at construction, before any secret is attached to anything.
func TestNewRefusesCleartextToAnotherHost(t *testing.T) {
	if _, err := New("http://hub.example/v1", "cred"); err == nil {
		t.Error("accepted plain http to a remote host")
	}
	if _, err := New("http://127.0.0.1:9/v1", "cred"); err != nil {
		t.Errorf("refused loopback http, which yad hub serve prints: %v", err)
	}
}

// A hub that redirects must not receive the credential at the new address: Go
// keeps Authorization on a same-host redirect even from https to http.
func TestRedirectIsRefusedAndCarriesNoCredential(t *testing.T) {
	leaked := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/moved/", func(w http.ResponseWriter, r *http.Request) {
		leaked <- r.Header.Get("Authorization")
	})
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/moved"+r.URL.Path, http.StatusTemporaryRedirect)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c, err := New(srv.URL+"/v1", "super-secret-credential")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Sync(context.Background(), "r", v1.SyncRequest{RunnerID: "r"})
	if err == nil || !strings.Contains(err.Error(), "must not redirect") {
		t.Errorf("err = %v, want a refused redirect", err)
	}
	select {
	case auth := <-leaked:
		t.Errorf("the redirect target received Authorization %q", auth)
	default:
	}
}

// A hub URL with userinfo is refused at New, so the one way left for a
// credential to reach an error is a redirect: the hub names a Location that
// carries one, the refusal quotes it, and Go's transport error quotes it again
// with only the password starred. Both reach `yad status` and the daemon's log.
func TestAnErrorNeverQuotesACredentialFromAURL(t *testing.T) {
	const secret = "sk-secret-token"
	for _, tc := range []struct{ name, userinfo string }{
		{"token as username", secret},
		{"token as password", "runner:" + secret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "http://"+tc.userinfo+"@"+r.Host+"/elsewhere", http.StatusTemporaryRedirect)
			}))
			t.Cleanup(redirect.Close)
			c, err := New(redirect.URL+"/v1", "a-token")
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Sync(context.Background(), "r1", v1.SyncRequest{})
			switch {
			case err == nil:
				t.Fatal("the call succeeded; it was meant to fail and quote a URL")
			case strings.Contains(err.Error(), secret):
				t.Errorf("the error carries the credential: %v", err)
			case !strings.Contains(err.Error(), "127.0.0.1:"):
				t.Errorf("the error no longer says where the hub pointed: %v", err)
			}
		})
	}
}
