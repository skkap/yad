package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/hub/store/db"
)

// Releasing a session (decision 0084), from the hub's side: the service API
// asks, the bound runner hears release_session until it reports, the archive
// goes up with PUT and down with GET only to the runners it is for, and a
// released session's next run opens it, with the archive, on another runner.

func archiveCall(t *testing.T, f *fixture, method, cred, session string, body []byte, sha string) (*httptest.ResponseRecorder, v1.ErrorEnvelope) {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/sessions/"+session+"/archive", bytes.NewReader(body))
	req.Header.Set(v1.HeaderProtocol, v1.Version)
	req.Header.Set("Authorization", "Bearer "+cred)
	if method == http.MethodPut {
		req.Header.Set("Content-Type", v1.ArchiveContentType)
		req.Header.Set(v1.HeaderArchiveSHA256, sha)
	}
	rec := serve(f.hub, req)
	var env v1.ErrorEnvelope
	if rec.Code >= 400 {
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s %s: error body is not the protocol envelope: %s", method, session, rec.Body)
		}
	}
	return rec, env
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func controlsOf(res v1.SyncResponse, kind v1.ControlKind) []v1.Control {
	var out []v1.Control
	for _, c := range res.Controls {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func (f *fixture) session(t *testing.T, tok, id string) hubapi.Session {
	t.Helper()
	var s hubapi.Session
	if code, e := f.api(t, "GET", "/sessions/"+id, tok, nil, &s); code != http.StatusOK {
		t.Fatalf("GET /sessions/%s: %d %+v", id, code, e)
	}
	return s
}

func TestASessionIsReleasedAndOpensOnAnotherRunner(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "ops")
	r1 := f.register(t, "r1")
	f.held(t, "r1", r1, "a", first("r1", 2))
	f.mustSync(t, "r1", r1, req("r1", 2, v1.HeldRun{RunID: "a", State: v1.RunRunning}))
	r2 := f.register(t, "r2")
	f.mustSync(t, "r2", r2, first("r2", 2))
	r3 := f.register(t, "r3")
	f.mustSync(t, "r3", r3, downgraded("r3", 2, capability.FeatureRelease))

	var s hubapi.Session
	if code, e := f.api(t, "POST", "/sessions/s-a/release", tok, hubapi.ReleaseRequest{DiscardLocal: true}, &s); code != http.StatusOK || s.Release != hubapi.SessionReleasing || s.State != hubapi.SessionOpen {
		t.Fatalf("release: %d %+v %+v", code, e, s)
	}
	f.enqueue(t, v1.Run{RunID: "b", Session: v1.SessionRef{ID: "s-a"}, Harness: "claude", Model: "opus", Brief: v1.Brief{Instruction: "go on"}})
	res := f.mustSync(t, "r1", r1, req("r1", 1, v1.HeldRun{RunID: "a", State: v1.RunRunning}))
	if c := controlsOf(res, v1.ControlReleaseSession); len(c) != 1 || c[0].SessionID != "s-a" || !c[0].DiscardLocal {
		t.Fatalf("release_session sent as %+v", c)
	}
	if len(res.Runs) != 0 || res.ArchiveMaxBytes != DefaultArchiveMaxBytes {
		t.Errorf("a releasing session's run was offered %v; archive_max_bytes %d", ids(res.Runs), res.ArchiveMaxBytes)
	}

	archive := []byte("an opaque archive")
	if rec, env := archiveCall(t, f, http.MethodPut, r2, "s-a", archive, sum(archive)); rec.Code != http.StatusForbidden || env.Error.Code != v1.CodeNotHolder {
		t.Errorf("an upload from a runner the session is not bound to: %d %+v", rec.Code, env)
	}
	if rec, env := archiveCall(t, f, http.MethodPut, r1, "s-a", archive, sum([]byte("other"))); rec.Code != http.StatusBadRequest || env.Error.Code != v1.CodeInvalid {
		t.Errorf("an upload whose bytes are not its sha256: %d %+v", rec.Code, env)
	}
	for range 2 {
		rec, env := archiveCall(t, f, http.MethodPut, r1, "s-a", archive, sum(archive))
		var got v1.SessionArchive
		json.Unmarshal(rec.Body.Bytes(), &got)
		if rec.Code != http.StatusOK || got.SHA256 != sum(archive) || got.Bytes != int64(len(archive)) {
			t.Fatalf("the upload: %d %+v %+v", rec.Code, env, got)
		}
	}
	if rec, _ := archiveCall(t, f, http.MethodGet, r2, "s-a", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("an archive whose release is not reported is handed out: %d", rec.Code)
	}

	// The run ended, and the release is reported: the session is on no
	// runner, and the run waiting in it goes out opening it, with the
	// archive, to a runner that opens one — not one it was released from.
	if err := f.store.FinishRun(t.Context(), db.FinishRunParams{State: "succeeded", UpdatedAt: f.clock.Now().UnixMilli(), ID: "a"}); err != nil {
		t.Fatal(err)
	}
	released := v1.ReleasedSession{SessionID: "s-a", ReleasedAt: f.clock.Now(), Archive: &v1.SessionArchive{SHA256: sum(archive), Bytes: int64(len(archive))}}
	rel := req("r1", 1)
	rel.ReleasedSessions = []v1.ReleasedSession{released}
	res = f.mustSync(t, "r1", r1, rel)
	if len(res.Runs) != 0 || len(controlsOf(res, v1.ControlReleaseSession)) != 0 {
		t.Fatalf("the runner that released the session was offered %v and %v", ids(res.Runs), res.Controls)
	}
	if s := f.session(t, tok, "s-a"); s.Release != hubapi.SessionReleased || s.RunnerID != "" || s.ReleasedFrom != "r1" || s.Archive == nil {
		t.Fatalf("the released session: %+v", s)
	}
	if got := f.mustSync(t, "r3", r3, downgraded("r3", 2, capability.FeatureRelease)); len(got.Runs) != 0 {
		t.Errorf("a runner without release was offered %v", ids(got.Runs))
	}
	if rec, env := archiveCall(t, f, http.MethodGet, r3, "s-a", nil, ""); rec.Code != http.StatusForbidden || env.Error.Code != v1.CodeNotHolder {
		t.Errorf("an archive fetched by a runner with no run of it: %d %+v", rec.Code, env)
	}
	res = f.mustSync(t, "r2", r2, req("r2", 2))
	if len(res.Runs) != 1 || res.Runs[0].RunID != "b" || !res.Runs[0].Session.New || res.Runs[0].Session.Archive == nil || res.Runs[0].Session.Archive.SHA256 != sum(archive) {
		t.Fatalf("r2 was offered %+v", res.Runs)
	}
	rec, _ := archiveCall(t, f, http.MethodGet, r2, "s-a", nil, "")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), archive) || rec.Header().Get(v1.HeaderArchiveSHA256) != sum(archive) || rec.Header().Get("Content-Type") != v1.ArchiveContentType {
		t.Fatalf("the archive handed to r2: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	f.mustSync(t, "r2", r2, req("r2", 1, claimed("b")...))
	if _, err := f.store.GetSessionArchive(t.Context(), "s-a"); err != nil {
		t.Fatalf("the archive went before a run opened from it was running: %v", err)
	}
	f.mustSync(t, "r2", r2, req("r2", 1, running("b")...))
	if _, err := f.store.GetSessionArchive(t.Context(), "s-a"); err == nil {
		t.Error("the archive stayed once a run opened from it was running")
	}
	if s := f.session(t, tok, "s-a"); s.RunnerID != "r2" || s.Release != "" {
		t.Errorf("the session after it opened on r2: %+v", s)
	}
}

// releasing is r1 holding a finished session s-a with a release asked for.
func releasing(t *testing.T, f *fixture, tok string) string {
	t.Helper()
	r1 := f.register(t, "r1")
	f.held(t, "r1", r1, "a", first("r1", 2))
	if err := f.store.FinishRun(t.Context(), db.FinishRunParams{State: "succeeded", UpdatedAt: f.clock.Now().UnixMilli(), ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if code, e := f.api(t, "POST", "/sessions/s-a/release", tok, hubapi.ReleaseRequest{}, nil); code != http.StatusOK {
		t.Fatalf("release: %d %+v", code, e)
	}
	return r1
}

// A release the runner refuses leaves the session on it, as it was: the
// error is shown, the control stops, and the session's runs go to it again.
func TestARefusedReleaseLeavesTheSessionOnItsRunner(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "ops")
	r1 := releasing(t, f, tok)
	f.enqueue(t, v1.Run{RunID: "b", Session: v1.SessionRef{ID: "s-a"}, Harness: "claude", Model: "opus", Brief: v1.Brief{Instruction: "go on"}})
	// What it uploaded before it refused is nobody's conversation.
	junk := []byte("half an archive")
	if rec, env := archiveCall(t, f, http.MethodPut, r1, "s-a", junk, sum(junk)); rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %+v", rec.Code, env)
	}
	why := v1.ReleaseError{Code: v1.ReleaseUnpushed, Message: "branch x holds 2 commits", NextAction: "push it"}
	r := req("r1", 1)
	r.ReleasedSessions = []v1.ReleasedSession{{SessionID: "s-a", ReleasedAt: f.clock.Now(), Error: &why}}
	res := f.mustSync(t, "r1", r1, r)
	if len(controlsOf(res, v1.ControlReleaseSession)) != 0 {
		t.Error("release_session went out again after the runner refused it")
	}
	if !slices.Equal(ids(res.Runs), []string{"b"}) || res.Runs[0].Session.New || res.Runs[0].Session.Archive != nil {
		t.Errorf("after the refusal r1 was offered %+v, want b continuing the session", res.Runs)
	}
	s := f.session(t, tok, "s-a")
	if s.Release != "" || s.RunnerID != "r1" || s.ReleaseError == nil || s.ReleaseError.Code != v1.ReleaseUnpushed || s.Archive != nil {
		t.Errorf("the session after a refusal: %+v", s)
	}
}

// The service API refuses what no runner would do: a session on no runner,
// or on a runner that does not advertise release for its harness. And a
// runner that stops advertising release while one is asked for has it ended
// for it, rather than holding the session's runs for ever.
func TestReleasingWhatCannotBeReleasedIs409(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "ops")
	r1 := f.register(t, "r1")
	f.held(t, "r1", r1, "a", downgraded("r1", 2, capability.FeatureRelease))
	f.enqueue(t, run("u", "s-unbound"))
	for _, tc := range []struct{ session, want string }{
		{"s-a", `"release" feature`},
		{"s-unbound", "on no runner"},
	} {
		code, e := f.api(t, "POST", "/sessions/"+tc.session+"/release", tok, hubapi.ReleaseRequest{}, nil)
		if code != http.StatusConflict || !strings.Contains(e.Message, tc.want) || e.NextAction == "" {
			t.Errorf("%s: %d %+v", tc.session, code, e)
		}
	}
	if code, _ := f.api(t, "POST", "/sessions/nope/release", tok, hubapi.ReleaseRequest{}, nil); code != http.StatusNotFound {
		t.Errorf("an unknown session: %d", code)
	}
	f2 := newFixture(t)
	tok2 := f2.admin(t, "ops")
	r := releasing(t, f2, tok2)
	if res := f2.mustSync(t, "r1", r, downgraded("r1", 1, capability.FeatureRelease)); len(controlsOf(res, v1.ControlReleaseSession)) != 0 {
		t.Error("release_session went to a runner no longer advertising release")
	}
	if s := f2.session(t, tok2, "s-a"); s.Release != "" || s.ReleaseError == nil || s.ReleaseError.Code != v1.ReleaseNotReleasable {
		t.Errorf("the session after its runner stopped advertising release: %+v", s)
	}
}

// A released session whose archive is not the one the hub holds is gone with
// its conversation; one whose archive outlives the TTL closes, since nothing
// else could open it. Either way its waiting runs end.
func TestAReleasedSessionWithNoArchiveToOpenCloses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		upload bool
		later  time.Duration
	}{
		{"reported with an archive the hub does not hold", false, 0},
		{"its archive outlived the TTL", true, DefaultArchiveTTL + time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tok := f.admin(t, "ops")
			r1 := releasing(t, f, tok)
			archive := []byte("the archive")
			if tc.upload {
				if rec, env := archiveCall(t, f, http.MethodPut, r1, "s-a", archive, sum(archive)); rec.Code != http.StatusOK {
					t.Fatalf("upload: %d %+v", rec.Code, env)
				}
			}
			r := req("r1", 1)
			r.ReleasedSessions = []v1.ReleasedSession{{SessionID: "s-a", ReleasedAt: f.clock.Now(), Archive: &v1.SessionArchive{SHA256: sum(archive), Bytes: int64(len(archive))}}}
			f.enqueue(t, v1.Run{RunID: "b", Session: v1.SessionRef{ID: "s-a"}, Harness: "claude", Model: "opus", Brief: v1.Brief{Instruction: "go on"}})
			f.mustSync(t, "r1", r1, r)
			f.clock.Advance(tc.later)
			if err := f.hub.Sweep(t.Context()); err != nil {
				t.Fatal(err)
			}
			if s := f.session(t, tok, "s-a"); s.State != hubapi.SessionClosed || s.Archive != nil {
				t.Errorf("the session: %+v", s)
			}
			if got := f.state(t, "b"); got != "failed" {
				t.Errorf("its waiting run is %s, want failed", got)
			}
		})
	}
}

// The archive's GET answers with the bytes, not JSON: the document says so.
func TestTheArchiveIsDeclaredAsBytes(t *testing.T) {
	get := New(Options{}).api.OpenAPI().Paths["/sessions/{session}/archive"].Get
	ok := get.Responses["200"]
	if ok == nil || len(ok.Content) != 1 || ok.Content[v1.ArchiveContentType] == nil || ok.Content[v1.ArchiveContentType].Schema.Format != "binary" {
		t.Fatalf("the GET's 200 is declared as %+v", ok)
	}
}

// A session moved twice goes back to neither runner it left: each closed it
// and keeps it closed, and would refuse a run opening it.
func TestAReleasedSessionGoesToNoRunnerItWasReleasedFrom(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "ops")
	r1 := releasing(t, f, tok)
	r2 := f.register(t, "r2")
	f.mustSync(t, "r2", r2, first("r2", 2))
	release := func(runner, cred string, archive []byte) {
		t.Helper()
		if rec, env := archiveCall(t, f, http.MethodPut, cred, "s-a", archive, sum(archive)); rec.Code != http.StatusOK {
			t.Fatalf("upload from %s: %d %+v", runner, rec.Code, env)
		}
		r := req(runner, 1)
		r.ReleasedSessions = []v1.ReleasedSession{{SessionID: "s-a", ReleasedAt: f.clock.Now(), Archive: &v1.SessionArchive{SHA256: sum(archive), Bytes: int64(len(archive))}}}
		f.mustSync(t, runner, cred, r)
	}
	release("r1", r1, []byte("first"))
	f.enqueue(t, v1.Run{RunID: "b", Session: v1.SessionRef{ID: "s-a"}, Harness: "claude", Model: "opus", Brief: v1.Brief{Instruction: "go on"}})
	if got := ids(f.mustSync(t, "r2", r2, req("r2", 2)).Runs); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("r2 was offered %v", got)
	}
	f.mustSync(t, "r2", r2, req("r2", 1, running("b")...))
	if err := f.store.FinishRun(t.Context(), db.FinishRunParams{State: "succeeded", UpdatedAt: f.clock.Now().UnixMilli(), ID: "b"}); err != nil {
		t.Fatal(err)
	}
	if code, e := f.api(t, "POST", "/sessions/s-a/release", tok, hubapi.ReleaseRequest{}, nil); code != http.StatusOK {
		t.Fatalf("the second release: %d %+v", code, e)
	}
	release("r2", r2, []byte("second"))
	f.enqueue(t, v1.Run{RunID: "c", Session: v1.SessionRef{ID: "s-a"}, Harness: "claude", Model: "opus", Brief: v1.Brief{Instruction: "and on"}})
	for _, r := range []struct{ id, cred string }{{"r1", r1}, {"r2", r2}} {
		if got := f.mustSync(t, r.id, r.cred, req(r.id, 2)); len(got.Runs) != 0 {
			t.Errorf("%s, which the session was released from, was offered %v", r.id, ids(got.Runs))
		}
	}
	r3 := f.register(t, "r3")
	if got := ids(f.mustSync(t, "r3", r3, first("r3", 2)).Runs); !slices.Equal(got, []string{"c"}) {
		t.Errorf("a third runner was offered %v, want c", got)
	}
}
