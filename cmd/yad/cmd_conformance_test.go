package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/hub"
	"github.com/skkap/yad/internal/hub/store"
)

// The command is a URL, a token and an exit code. What the rules are, and
// whether `yad hub` keeps them, is internal/hub's own test; this is the
// operator typing it — including the token on stdin, which is how a secret
// stays off the terminal (decision 0020).
func TestConformanceChecksAHubAndSaysWhatItCouldNot(t *testing.T) {
	p := newProfile(t)
	code, tok, errs := p.yad("", "hub", "token", "create", "--ttl", "10m")
	if code != 0 {
		t.Fatalf("token create: exit %d: %s", code, errs)
	}
	tok = strings.TrimSpace(tok)
	s, err := store.Open(context.Background(), filepath.Join(p.data, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	srv := httptest.NewServer(hub.New(hub.Options{Store: s}))
	defer srv.Close()

	// No run is queued for the suite's harness, so the rules that need one
	// are reported as unchecked rather than quietly passing.
	code, out, errs := p.yad(tok+"\n", "conformance", srv.URL+hub.BasePath, "--token", "-", "--lease-wait", "0")
	if code != 0 {
		t.Fatalf("`yad hub` broke a rule of its own protocol: exit %d\n%s%s", code, out, errs)
	}
	// The report wraps its prose to the terminal, so a line of it is matched
	// with the spacing flattened rather than as it happens to have broken.
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"PASS register/exchange",
		"PASS sync/cancel-for-a-run-not-held",
		"Not checked: no run was offered",
		"queue one or two runs for that harness",
		"POST /runners/{runner}/deregister",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out+errs, tok) {
		t.Error("the registration token is in what the command printed")
	}
}

// Against something that is not a hub — a proxy answering every path the same
// way — the command exits non-zero and every failure names the rule and where
// it is written, which is the whole product.
func TestConformanceFailsAndNamesTheRule(t *testing.T) {
	p := newProfile(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	code, out, _ := p.yad("", "conformance", srv.URL+"/v1", "--token", "no-hub-issued-this", "--lease-wait", "0")
	if code != 1 {
		t.Fatalf("exit %d against a server that is no hub:\n%s", code, out)
	}
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{"FAIL errors/unknown-path", "ARCHITECTURE.md §2, Calls", "Observed:"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}
