package hub

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// Service API documents are what yashiki generates its client from; a drift
// between them and the Go types is a failed build, as for the protocol's.
func TestServiceOpenAPIIsCurrent(t *testing.T) {
	gen, err := ServiceOpenAPIYAML()
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile("../../protocol/hubapi/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, body, ok := bytes.Cut(committed, []byte("`make generate`.\n")); !ok || !bytes.Equal(body, gen) {
		t.Fatal("protocol/hubapi/openapi.yaml is stale — run `make generate` and commit the diff")
	}
}

func (f *fixture) admin(t *testing.T, name string) string {
	t.Helper()
	tok, err := IssueAdminToken(context.Background(), f.store, name, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// api calls the service API and decodes a 2xx answer into out, or returns
// the error envelope.
func (f *fixture) api(t *testing.T, method, path, token string, body any, out any) (int, v1.Error) {
	t.Helper()
	var rd io.Reader
	if raw, ok := body.(string); ok {
		// A string is sent as is, so a test can send what JSON cannot encode.
		rd = strings.NewReader(raw)
	} else if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, hubapi.BasePath+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := serve(f.hub, req)
	if rec.Code >= 400 {
		var env v1.ErrorEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.NextAction == "" {
			t.Fatalf("%s %s: %d is not the error envelope with a next action: %s", method, path, rec.Code, rec.Body)
		}
		return rec.Code, env.Error
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, rec.Body)
		}
	}
	return rec.Code, v1.Error{}
}

func submission(instruction string) hubapi.SubmitRequest {
	return hubapi.SubmitRequest{Harness: "claude", Model: "opus", Brief: v1.Brief{Instruction: instruction}}
}

// Only an admin token opens the service API, and an admin token opens nothing
// in the protocol: a runner can never submit work, and a service can never
// pose as a runner.
func TestServiceNeedsAnAdminToken(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	good := f.admin(t, "yashiki")
	revoked := f.admin(t, "old")
	if err := RevokeAdminToken(context.Background(), f.store, "old"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"none", "", 401},
		{"runner credential", cred, 401},
		{"revoked", revoked, 401},
		{"unknown", "yadadm_nope", 401},
		{"admin token", good, 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := f.api(t, "POST", "/runs", tc.token, submission("hi"), nil)
			if code != tc.want {
				t.Errorf("status %d, want %d", code, tc.want)
			}
		})
	}
	// Refused before routing, so a caller without a token learns nothing
	// about which paths exist.
	if code, _ := f.api(t, "GET", "/nowhere", "", nil, nil); code != 401 {
		t.Errorf("unknown path without a token: %d, want 401", code)
	}
	res, _ := f.sync(t, "r1", good, first("r1", 1))
	if res.StatusCode != 401 {
		t.Errorf("sync with an admin token: %d, want 401", res.StatusCode)
	}
}

// Every error the service API returns — its own or huma's — has the envelope,
// and a next action that points at the service API's document, never the
// runner protocol's: a service caller has no reason to read that one.
func TestServiceErrorsHaveTheEnvelope(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	for _, tc := range []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"unknown path", "GET", "/nowhere", nil, 404},
		{"wrong method", "DELETE", "/runs", nil, 405},
		{"unknown run", "GET", "/runs/nope", nil, 404},
		{"unknown run's events", "GET", "/runs/nope/events?wait_ms=0", nil, 404},
		{"negative cursor", "GET", "/runs/nope/events?after=-1", nil, 422},
		{"empty harness", "POST", "/runs", hubapi.SubmitRequest{Model: "opus", Brief: v1.Brief{Instruction: "x"}}, 422},
		{"malformed body", "POST", "/runs", `{`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, e := f.api(t, tc.method, tc.path, tok, tc.body, nil)
			if code != tc.want {
				t.Errorf("status %d, want %d", code, tc.want)
			}
			if strings.Contains(e.NextAction, "protocol/v1") || strings.Contains(e.NextAction, "registration token") {
				t.Errorf("next action sends a service caller to the runner protocol: %q", e.NextAction)
			}
		})
	}
	// The protocol's own errors keep pointing at the protocol.
	_, env := post(t, f.hub, "/v1/runs/r/events", `{`, headers(""))
	if !strings.Contains(env.Error.NextAction, "protocol/v1/openapi.yaml") {
		t.Errorf("protocol error next action %q", env.Error.NextAction)
	}
}

func TestSubmit(t *testing.T) {
	type step struct {
		req  hubapi.SubmitRequest
		want int
	}
	with := func(mod func(*hubapi.SubmitRequest)) hubapi.SubmitRequest {
		r := submission("do it")
		mod(&r)
		return r
	}
	newIn := func(id string) func(*hubapi.SubmitRequest) {
		return func(r *hubapi.SubmitRequest) { r.Session = &hubapi.SessionChoice{ID: id, New: true} }
	}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"new session with a generated id", []step{{submission("do it"), 201}}},
		{"continue a session", []step{
			{with(newIn("s1")), 201},
			{with(func(r *hubapi.SubmitRequest) { r.Session = &hubapi.SessionChoice{ID: "s1"} }), 201},
		}},
		{"continue a session the hub does not have", []step{
			{with(func(r *hubapi.SubmitRequest) { r.Session = &hubapi.SessionChoice{ID: "s1"} }), 404},
		}},
		{"start a session that exists", []step{{with(newIn("s1")), 201}, {with(newIn("s1")), 409}}},
		{"another harness in a session", []step{
			{with(newIn("s1")), 201},
			{with(func(r *hubapi.SubmitRequest) { r.Session = &hubapi.SessionChoice{ID: "s1"}; r.Harness = "codex" }), 409},
		}},
		{"retry with the same content", []step{
			{with(func(r *hubapi.SubmitRequest) { r.RunID = "r1" }), 201},
			{with(func(r *hubapi.SubmitRequest) { r.RunID = "r1" }), 201},
		}},
		{"retry into a named new session", []step{
			{with(func(r *hubapi.SubmitRequest) { r.RunID = "r1"; newIn("s1")(r) }), 201},
			{with(func(r *hubapi.SubmitRequest) { r.RunID = "r1"; newIn("s1")(r) }), 201},
		}},
		{"retry without the session it continued", []step{
			{with(newIn("s1")), 201},
			{with(func(r *hubapi.SubmitRequest) { r.RunID = "r1"; r.Session = &hubapi.SessionChoice{ID: "s1"} }), 201},
			{with(func(r *hubapi.SubmitRequest) { r.RunID = "r1" }), 409},
		}},
		{"retry without the session it started", []step{
			{with(func(r *hubapi.SubmitRequest) { r.RunID = "r1"; newIn("s1")(r) }), 201},
			{with(func(r *hubapi.SubmitRequest) { r.RunID = "r1" }), 201},
		}},
		{"reuse a run id for other content", []step{
			{with(func(r *hubapi.SubmitRequest) { r.RunID = "r1" }), 201},
			{with(func(r *hubapi.SubmitRequest) { r.RunID = "r1"; r.Brief.Instruction = "something else" }), 409},
		}},
		{"no instruction", []step{{with(func(r *hubapi.SubmitRequest) { r.Brief.Instruction = "" }), 400}}},
		{"no model", []step{{with(func(r *hubapi.SubmitRequest) { r.Model = "" }), 422}}},
		{"path-shaped run id", []step{{with(func(r *hubapi.SubmitRequest) { r.RunID = "../x" }), 400}}},
		{"source with neither git nor path", []step{{with(func(r *hubapi.SubmitRequest) { r.Sources = []v1.Source{{}} }), 400}}},
		// Grant names: any valid one, except the four that would break the
		// run, in any case (decision 0038).
		{"a grant the old rules refused", []step{{with(func(r *hubapi.SubmitRequest) {
			r.Grants = []v1.Grant{{Name: "DATABASE_URL", Value: "postgres://", As: v1.GrantEnv}}
		}), 201}}},
		{"a grant that would replace the path", []step{{with(func(r *hubapi.SubmitRequest) {
			r.Grants = []v1.Grant{{Name: "path", Value: "/tmp", As: v1.GrantFile}}
		}), 400}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tok := f.admin(t, "cli")
			var runs []hubapi.Run
			for i, s := range tc.steps {
				var got hubapi.Run
				code, e := f.api(t, "POST", "/runs", tok, s.req, &got)
				if code != s.want {
					t.Fatalf("step %d: status %d (%s), want %d", i, code, e.Message, s.want)
				}
				if code == 201 {
					runs = append(runs, got)
				}
			}
			for _, r := range runs {
				if r.RunID == "" || r.SessionID == "" || r.State != hubapi.RunQueued || r.Harness == "" || r.Model == "" {
					t.Errorf("answered %+v", r)
				}
			}
			if len(runs) == 2 && runs[0].RunID == runs[1].RunID && runs[0].SessionID != runs[1].SessionID {
				t.Errorf("a retry landed in another session: %s, %s", runs[0].SessionID, runs[1].SessionID)
			}
		})
	}
}

// A submitted run reaches a runner exactly as submitted, grants included — and
// the service API never hands a grant back.
func TestSubmittedRunIsOfferedAndGrantsStayHidden(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	req := submission("fix it")
	req.Brief.Context = "you are in the yad repo"
	req.Grants = []v1.Grant{{Name: "ZUMINO_TOKEN", Value: "grant-secret-value", As: v1.GrantEnv}}
	var sub hubapi.Run
	if code, e := f.api(t, "POST", "/runs", tok, req, &sub); code != 201 {
		t.Fatalf("submit: %d %s", code, e.Message)
	}

	res := f.mustSync(t, "r1", cred, first("r1", 1))
	if len(res.Runs) != 1 {
		t.Fatalf("offered %v", ids(res.Runs))
	}
	got := res.Runs[0]
	if got.RunID != sub.RunID || got.Session.ID != sub.SessionID || !got.Session.New || got.Session.Mode != v1.SessionPerRun ||
		got.Brief != req.Brief || len(got.Grants) != 1 || got.Grants[0].Value != "grant-secret-value" {
		t.Errorf("offered %+v", got)
	}

	for _, path := range []string{"/runs/" + sub.RunID, "/runs/" + sub.RunID + "/events?wait_ms=0"} {
		r := httptest.NewRequest("GET", hubapi.BasePath+path, nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		rec := serve(f.hub, r)
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "grant-secret-value") || strings.Contains(rec.Body.String(), "ZUMINO_TOKEN") {
			t.Errorf("GET %s: %d %s", path, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), `"state":"offered"`) {
			t.Errorf("GET %s does not show the offer: %s", path, rec.Body)
		}
	}
}

// event stores an event as the protocol's events call does — acked_through
// advanced over it — without a runner holding the run.
func (f *fixture) event(t *testing.T, runID string, seq int64, text string) {
	t.Helper()
	b, err := json.Marshal(v1.Event{Seq: seq, At: f.clock.Now(), Kind: v1.EventText, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = f.store.Tx(ctx, func(q *db.Queries) error {
		if _, err := q.AppendEvent(ctx, db.AppendEventParams{RunID: runID, Seq: seq, Body: string(b), ReceivedAt: store.Ms(f.clock.Now())}); err != nil {
			return err
		}
		run, err := q.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		_, err = advance(ctx, q, run)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.hub.Changed()
}

// A watcher reads a stream only as far as it is contiguous: an event stored
// past a gap is held back until the gap fills, so the cursor never moves past
// a seq that arrives later.
func TestEventPageStopsAtAGap(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	f.enqueue(t, run("a", "s1"))
	f.event(t, "a", 1, "one")
	f.event(t, "a", 2, "two")
	f.event(t, "a", 4, "four")
	f.finish(t, "a", v1.RunSucceeded, &v1.Result{State: v1.RunSucceeded, LastSeq: 4})
	p := f.page(t, tok, "a", 0, 0)
	if !slices.Equal(seqs(p), []int64{1, 2}) || p.NextAfter != 2 || p.Done {
		t.Fatalf("before the gap fills: seqs %v next %d done %v", seqs(p), p.NextAfter, p.Done)
	}
	f.event(t, "a", 3, "three")
	p = f.page(t, tok, "a", p.NextAfter, 0)
	if !slices.Equal(seqs(p), []int64{3, 4}) || !p.Done {
		t.Errorf("after the gap fills: seqs %v done %v", seqs(p), p.Done)
	}
}

// finish ends a run as a result upload will; a nil result is a run lost with
// no report.
func (f *fixture) finish(t *testing.T, runID string, state v1.RunState, res *v1.Result) {
	t.Helper()
	ctx := context.Background()
	if res != nil {
		b, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.PutResult(ctx, db.PutResultParams{RunID: runID, State: string(state), Body: string(b), ReceivedAt: store.Ms(f.clock.Now())}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.DB.ExecContext(ctx, "UPDATE runs SET state = ? WHERE id = ?", string(state), runID); err != nil {
		t.Fatal(err)
	}
	f.hub.Changed()
}

func (f *fixture) page(t *testing.T, tok, runID string, after int64, wait time.Duration) hubapi.EventPage {
	t.Helper()
	var p hubapi.EventPage
	path := fmt.Sprintf("/runs/%s/events?after=%d&wait_ms=%d", runID, after, wait.Milliseconds())
	if code, e := f.api(t, "GET", path, tok, nil, &p); code != 200 {
		t.Fatalf("events: %d %s", code, e.Message)
	}
	return p
}

func seqs(p hubapi.EventPage) []int64 {
	var out []int64
	for _, e := range p.Events {
		out = append(out, e.Seq)
	}
	return out
}

// Done is the end of the stream, not the end of the run: a result may arrive
// before the runner's last batch of events, and a watcher that stopped at the
// result would miss them.
func TestEventPageIsDoneOnlyWithEveryEvent(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	f.enqueue(t, run("a", "s1"), run("lost", "s2"))

	p := f.page(t, tok, "a", 0, 0)
	if len(p.Events) != 0 || p.Done || p.NextAfter != 0 || p.Run.State != hubapi.RunQueued {
		t.Errorf("empty run: %+v", p)
	}
	f.event(t, "a", 1, "one")
	f.finish(t, "a", v1.RunSucceeded, &v1.Result{State: v1.RunSucceeded, LastSeq: 2, FinalText: "done"})
	p = f.page(t, tok, "a", 0, 0)
	if !slices.Equal(seqs(p), []int64{1}) || p.Done || p.NextAfter != 1 {
		t.Errorf("result ahead of its last event: seqs %v done %v next %d", seqs(p), p.Done, p.NextAfter)
	}
	if p.Run.Result == nil || p.Run.Result.FinalText != "done" || p.Run.State != hubapi.RunState(v1.RunSucceeded) {
		t.Errorf("run = %+v", p.Run)
	}
	f.event(t, "a", 2, "two")
	p = f.page(t, tok, "a", 1, 0)
	if !slices.Equal(seqs(p), []int64{2}) || !p.Done || p.NextAfter != 2 {
		t.Errorf("last page: seqs %v done %v next %d", seqs(p), p.Done, p.NextAfter)
	}

	// A lost run has no result and ends with what came.
	f.finish(t, "lost", v1.RunLost, nil)
	if p := f.page(t, tok, "lost", 0, 0); !p.Done || p.Run.Result != nil {
		t.Errorf("lost run: %+v", p)
	}
}

func TestEventPagesAreBounded(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	f.enqueue(t, run("a", "s1"))
	n := int64(hubapi.MaxPage + 3)
	for i := int64(1); i <= n; i++ {
		f.event(t, "a", i, "x")
	}
	f.finish(t, "a", v1.RunSucceeded, &v1.Result{State: v1.RunSucceeded, LastSeq: n})
	p := f.page(t, tok, "a", 0, 0)
	if len(p.Events) != hubapi.MaxPage || p.Done || p.NextAfter != hubapi.MaxPage {
		t.Errorf("first page: %d events, done %v, next %d", len(p.Events), p.Done, p.NextAfter)
	}
	p = f.page(t, tok, "a", p.NextAfter, 0)
	if len(p.Events) != 3 || !p.Done || p.NextAfter != n {
		t.Errorf("second page: %d events, done %v, next %d", len(p.Events), p.Done, p.NextAfter)
	}
}

// A waiting poll answers as soon as there is something to say — an event, a
// state change — and otherwise at the end of its wait, empty.
func TestEventsLongPoll(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(f *fixture, t *testing.T)
		check func(t *testing.T, p hubapi.EventPage)
	}{
		{"an event arrives", func(f *fixture, t *testing.T) { f.event(t, "a", 1, "hello") },
			func(t *testing.T, p hubapi.EventPage) {
				if !slices.Equal(seqs(p), []int64{1}) {
					t.Errorf("seqs %v", seqs(p))
				}
			}},
		{"the state moves", func(f *fixture, t *testing.T) {
			if _, err := f.store.DB.Exec("UPDATE runs SET state = 'running' WHERE id = 'a'"); err != nil {
				t.Error(err)
			}
			f.hub.Changed()
		}, func(t *testing.T, p hubapi.EventPage) {
			if p.Run.State != hubapi.RunState(v1.RunRunning) || len(p.Events) != 0 {
				t.Errorf("page %+v", p)
			}
		}},
		{"the run is lost", func(f *fixture, t *testing.T) { f.finish(t, "a", v1.RunLost, nil) },
			func(t *testing.T, p hubapi.EventPage) {
				if !p.Done {
					t.Errorf("page %+v", p)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tok := f.admin(t, "cli")
			f.enqueue(t, run("a", "s1"))
			got := make(chan hubapi.EventPage, 1)
			start := time.Now()
			go func() { got <- f.page(t, tok, "a", 0, 20*time.Second) }()
			// The poll must be waiting before the write, or this tests nothing.
			waitForWaiter(t, f.hub)
			tc.write(f, t)
			select {
			case p := <-got:
				tc.check(t, p)
				if time.Since(start) > 10*time.Second {
					t.Errorf("answered after %s", time.Since(start))
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the poll did not answer")
			}
		})
	}

	t.Run("nothing happens", func(t *testing.T) {
		f := newFixture(t)
		tok := f.admin(t, "cli")
		f.enqueue(t, run("a", "s1"))
		start := time.Now()
		p := f.page(t, tok, "a", 0, 300*time.Millisecond)
		if len(p.Events) != 0 || p.Done || time.Since(start) < 300*time.Millisecond {
			t.Errorf("page %+v after %s", p, time.Since(start))
		}
	})

	// A caller that hangs up frees the request at once.
	t.Run("the caller leaves", func(t *testing.T) {
		f := newFixture(t)
		tok := f.admin(t, "cli")
		f.enqueue(t, run("a", "s1"))
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequestWithContext(ctx, "GET", hubapi.BasePath+"/runs/a/events?wait_ms=20000", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		done := make(chan struct{})
		go func() { serve(f.hub, req); close(done) }()
		waitForWaiter(t, f.hub)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the request outlived its caller")
		}
	})
}

// waitForWaiter blocks until an events request is parked on the bell.
func waitForWaiter(t *testing.T, h *Hub) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.bell.mu.Lock()
		parked := h.bell.ch != nil
		h.bell.mu.Unlock()
		if parked {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no events request started waiting")
}

func TestAdminTokens(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tok := f.admin(t, "cli")
	if !strings.HasPrefix(tok, adminTokenPrefix) {
		t.Errorf("token %q lacks its prefix", tok[:4])
	}
	if _, err := IssueAdminToken(ctx, f.store, "cli", f.clock.Now()); err == nil {
		t.Error("a second token took an existing name")
	}
	if _, err := IssueAdminToken(ctx, f.store, "Bad Name", f.clock.Now()); err == nil {
		t.Error("a name with spaces was accepted")
	}
	var stored string
	if err := f.store.DB.QueryRow("SELECT hash FROM admin_tokens WHERE name = 'cli'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == tok || stored != hashSecret(tok) {
		t.Error("the token is not stored as its hash")
	}
	if err := RevokeAdminToken(ctx, f.store, "cli"); err != nil {
		t.Fatal(err)
	}
	if err := RevokeAdminToken(ctx, f.store, "cli"); err == nil {
		t.Error("revoking a missing token succeeded")
	}
	if _, err := f.store.GetAdminToken(ctx, hashSecret(tok)); err != sql.ErrNoRows {
		t.Errorf("revoked token still found: %v", err)
	}
}
