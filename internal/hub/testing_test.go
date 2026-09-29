package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/hub/store"
)

// clock is a settable time source, so leases lapse without a sleep.
type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

type fixture struct {
	hub   *Hub
	store *store.Store
	clock *clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &clock{t: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
	return &fixture{hub: New(Options{Store: s, Now: c.Now}), store: s, clock: c}
}

func (f *fixture) token(t *testing.T, ttl time.Duration) string {
	t.Helper()
	return f.tokenFor(t, ttl, "")
}

func (f *fixture) tokenFor(t *testing.T, ttl time.Duration, runner string) string {
	t.Helper()
	tok, _, err := IssueRegistrationToken(context.Background(), f.store, ttl, f.clock.Now(), runner)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// doc is a capability document for a runner that can drive the fake harness
// "claude", as a first-class one would be advertised, and that advertises
// everything this build of the runner acts on — what a current yad sends.
func doc(id string) v1.Capabilities {
	harnesses := []v1.HarnessReport{
		{ID: "claude", Label: "Claude Code", Kind: "first-class", Present: true, Version: "2.1.276", Features: capability.HarnessFeatures("claude")},
		{ID: "codex", Label: "Codex", Kind: "recognised", Present: true, Version: "0.1"},
	}
	return v1.Capabilities{
		RunnerID: id, Name: id, YadVersion: "dev", OS: "linux", Arch: "amd64",
		ProtocolFeatures: capability.Features(harnesses),
		Harnesses:        harnesses,
		Capacity:         v1.Capacity{Total: 4},
	}
}

func run(id, session string) v1.Run {
	return v1.Run{RunID: id, Session: v1.SessionRef{ID: session, New: true}, Harness: "claude", Model: "opus", Brief: v1.Brief{Instruction: "do " + id}}
}

func newRequest(t *testing.T, path, body string, headers map[string]string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// register registers a runner that drives claude and returns its credential;
// a runner the hub already knows is re-registered with a token issued for it.
func (f *fixture) register(t *testing.T, id string) string {
	t.Helper()
	tok := f.token(t, time.Hour)
	if _, err := f.store.GetRunner(context.Background(), id); err == nil {
		tok = f.tokenFor(t, time.Hour, id)
	}
	rec := serve(f.hub, newRequest(t, "/v1/runners/register", registerBody(t, doc(id)), headers(tok)))
	if rec.Code != http.StatusOK {
		t.Fatalf("register %s: %d %s", id, rec.Code, rec.Body)
	}
	var res v1.RegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res.RunnerCredential
}

func (f *fixture) sync(t *testing.T, runner, cred string, req v1.SyncRequest) (*http.Response, v1.ErrorEnvelope) {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return post(t, f.hub, "/v1/runners/"+runner+"/sync", string(b), headers(cred))
}

// mustSync syncs and decodes the answer, failing on anything but 200.
func (f *fixture) mustSync(t *testing.T, runner, cred string, req v1.SyncRequest) v1.SyncResponse {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := serve(f.hub, newRequest(t, "/v1/runners/"+runner+"/sync", string(b), headers(cred)))
	if rec.Code != http.StatusOK {
		t.Fatalf("sync %s: %d %s", runner, rec.Code, rec.Body)
	}
	var res v1.SyncResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func (f *fixture) enqueue(t *testing.T, runs ...v1.Run) {
	t.Helper()
	for _, r := range runs {
		if err := f.store.EnqueueRun(context.Background(), r, f.clock.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fixture) state(t *testing.T, runID string) string {
	t.Helper()
	r, err := f.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return r.State
}
