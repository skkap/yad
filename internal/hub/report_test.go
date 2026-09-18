package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub/store/db"
)

// claimedBy puts run id in a new session, offers it to runner and has runner
// list it back: the state in which the runner may report on it.
func (f *fixture) claimedBy(t *testing.T, runner, cred, id string) {
	t.Helper()
	f.enqueue(t, run(id, "s-"+id))
	res := f.mustSync(t, runner, cred, first(runner, 1))
	if len(res.Runs) != 1 || res.Runs[0].RunID != id {
		t.Fatalf("offer: %v", ids(res.Runs))
	}
	f.mustSync(t, runner, cred, req(runner, 0, claimed(id)...))
	if f.state(t, id) != "claimed" {
		t.Fatalf("state %s after listing", f.state(t, id))
	}
}

func batch(seqs ...int64) v1.EventBatch {
	b := v1.EventBatch{Events: []v1.Event{}}
	for _, s := range seqs {
		b.Events = append(b.Events, v1.Event{Seq: s, At: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC), Kind: v1.EventText, Text: fmt.Sprint("event ", s)})
	}
	return b
}

func (f *fixture) events(t *testing.T, cred, runID string, b v1.EventBatch) (int, v1.EventAck, v1.ErrorEnvelope) {
	t.Helper()
	body, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	rec := serve(f.hub, newRequest(t, "/v1/runs/"+runID+"/events", string(body), headers(cred)))
	var ack v1.EventAck
	var env v1.ErrorEnvelope
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
			t.Fatal(err)
		}
	} else if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%d with a body that is not the envelope: %s", rec.Code, rec.Body)
	}
	return rec.Code, ack, env
}

func (f *fixture) result(t *testing.T, cred, runID string, res v1.Result) (int, v1.ErrorEnvelope) {
	t.Helper()
	body, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	rec := serve(f.hub, newRequest(t, "/v1/runs/"+runID+"/result", string(body), headers(cred)))
	var env v1.ErrorEnvelope
	if rec.Code != http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%d with a body that is not the envelope: %s", rec.Code, rec.Body)
		}
	}
	return rec.Code, env
}

// acked_through is how far the events are contiguous from 1: a gap holds it,
// a resend is ignored, and filling the gap moves it past everything held.
func TestEventsAckedThrough(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.claimedBy(t, "r1", cred, "a")
	for _, tc := range []struct {
		name string
		b    v1.EventBatch
		want int64
	}{
		{"in order", batch(1, 2, 3), 3},
		{"a resend", batch(2, 3), 3},
		{"after a gap", batch(5, 6), 3},
		{"the gap filled", batch(4), 6},
		{"an empty batch", batch(), 6},
	} {
		code, ack, env := f.events(t, cred, "a", tc.b)
		if code != http.StatusOK || ack.AckedThrough != tc.want {
			t.Errorf("%s: %d acked_through %d, want %d (%+v)", tc.name, code, ack.AckedThrough, tc.want, env.Error)
		}
	}
	// The first copy of an event stands.
	b := batch(1)
	b.Events[0].Text = "rewritten"
	f.events(t, cred, "a", b)
	rows, err := f.store.EventsAfter(context.Background(), eventsAfter("a"))
	if err != nil || len(rows) != 6 || strings.Contains(rows[0].Body, "rewritten") {
		t.Errorf("stored %d events, first %v, %v", len(rows), rows[0].Body, err)
	}
}

// Only the runner a run was claimed by may write its events or its result. A
// runner that lost the race — or never had the run — hears not_holder, and
// nothing it sent is applied.
func TestOnlyTheHolderReports(t *testing.T) {
	f := newFixture(t)
	c1 := f.register(t, "r1")
	c2 := f.register(t, "r2")
	f.claimedBy(t, "r1", c1, "a")
	f.enqueue(t, run("q", "s-q"))

	for _, tc := range []struct {
		name, cred, run string
		status          int
		code            string
	}{
		{"events from another runner", c2, "a", 403, v1.CodeNotHolder},
		{"events for a queued run", c1, "q", 403, v1.CodeNotHolder},
		{"events for an unknown run", c1, "nope", 404, v1.CodeNotFound},
		{"events with no credential", "", "a", 401, v1.CodeUnauthorized},
		{"events with an unknown credential", "not-a-credential", "a", 401, v1.CodeUnauthorized},
	} {
		code, _, env := f.events(t, tc.cred, tc.run, batch(1))
		if code != tc.status || env.Error.Code != tc.code || env.Error.NextAction == "" {
			t.Errorf("%s: %d %+v", tc.name, code, env.Error)
		}
	}
	for _, tc := range []struct {
		name, cred, run string
		status          int
		code            string
	}{
		{"result from another runner", c2, "a", 403, v1.CodeNotHolder},
		{"result for a queued run", c1, "q", 403, v1.CodeNotHolder},
		{"result for an unknown run", c1, "nope", 404, v1.CodeNotFound},
	} {
		code, env := f.result(t, tc.cred, tc.run, v1.Result{State: v1.RunSucceeded})
		if code != tc.status || env.Error.Code != tc.code || env.Error.NextAction == "" {
			t.Errorf("%s: %d %+v", tc.name, code, env.Error)
		}
	}
	if f.state(t, "a") != "claimed" || f.state(t, "q") != "queued" {
		t.Errorf("states changed by refused calls: a %s, q %s", f.state(t, "a"), f.state(t, "q"))
	}
	if rows, _ := f.store.EventsAfter(context.Background(), eventsAfter("a")); len(rows) != 0 {
		t.Errorf("a refused batch stored %d events", len(rows))
	}
}

// A result is applied once: the same state again is acknowledged, a different
// one is 409 and changes nothing, and late events from the holder still land.
func TestResultAppliedAtMostOnce(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.claimedBy(t, "r1", cred, "a")
	res := v1.Result{State: v1.RunFailed, Error: &v1.RunError{Class: "harness_error", Message: "it broke"}, LastSeq: 2}
	if code, env := f.result(t, cred, "a", res); code != 200 {
		t.Fatalf("result: %d %+v", code, env.Error)
	}
	run, err := f.store.GetRun(context.Background(), "a")
	if err != nil || run.State != "failed" || run.Reason.String != "it broke" || run.LeaseExpiresAt.Valid {
		t.Fatalf("run after result: %+v %v", run, err)
	}
	if code, _ := f.result(t, cred, "a", res); code != 200 {
		t.Errorf("the same result again: %d", code)
	}
	code, env := f.result(t, cred, "a", v1.Result{State: v1.RunSucceeded})
	if code != 409 || env.Error.Code != v1.CodeConflict || env.Error.NextAction == "" {
		t.Errorf("a different result: %d %+v", code, env.Error)
	}
	if f.state(t, "a") != "failed" {
		t.Errorf("state after a conflicting result: %s", f.state(t, "a"))
	}
	if code, ack, _ := f.events(t, cred, "a", batch(1, 2)); code != 200 || ack.AckedThrough != 2 {
		t.Errorf("late events from the holder: %d %d", code, ack.AckedThrough)
	}
	// A finished run is not the runner's to hold: listing it is answered
	// with cancel and does not reopen it.
	out := f.mustSync(t, "r1", cred, req("r1", 1, claimed("a")...))
	if got := cancels(out); len(got) != 1 || got[0] != "a" || f.state(t, "a") != "failed" {
		t.Errorf("listing a finished run: cancels %v, state %s", got, f.state(t, "a"))
	}
}

// A runner back from a partition reports a result for a run the hub already
// marked lost. Lost stands (decision 0023); a result that agrees is recorded.
func TestLostStandsAgainstALateResult(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.claimedBy(t, "r1", cred, "a")
	f.claimedBy(t, "r1", cred, "b")
	f.clock.Advance(time.Hour)
	if err := f.hub.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.state(t, "a") != "lost" {
		t.Fatalf("state after the lease lapsed: %s", f.state(t, "a"))
	}
	code, env := f.result(t, cred, "a", v1.Result{State: v1.RunSucceeded})
	if code != 409 || env.Error.Code != v1.CodeConflict || !strings.Contains(env.Error.Message, "lost") {
		t.Errorf("late result: %d %+v", code, env.Error)
	}
	if f.state(t, "a") != "lost" {
		t.Errorf("state after a late result: %s", f.state(t, "a"))
	}
	if code, _ := f.result(t, cred, "b", v1.Result{State: v1.RunLost}); code != 200 {
		t.Errorf("a result that agrees with lost: %d", code)
	}
	if _, err := f.store.GetResult(context.Background(), "b"); err != nil {
		t.Errorf("the agreeing result was not recorded: %v", err)
	}
	// The stream is still the holder's to complete, for diagnosis.
	if code, _, _ := f.events(t, cred, "a", batch(1)); code != 200 {
		t.Errorf("events for a lost run from its holder: %d", code)
	}
}

// A runner refuses a run it was offered with a failed result (decision 0019);
// the hub takes it from the runner the run was offered to, and the run stops
// being offered.
func TestRefusalOfAnOffer(t *testing.T) {
	f := newFixture(t)
	c1 := f.register(t, "r1")
	c2 := f.register(t, "r2")
	f.enqueue(t, run("a", "s1"))
	f.mustSync(t, "r1", c1, first("r1", 1))
	if f.state(t, "a") != "offered" {
		t.Fatalf("state %s", f.state(t, "a"))
	}
	if code, _, _ := f.events(t, c1, "a", batch(1)); code != 403 {
		t.Errorf("events for a run only offered: %d", code)
	}
	refusal := v1.Result{State: v1.RunFailed, Error: &v1.RunError{Class: "refused", Message: "cannot drive it"}}
	if code, _ := f.result(t, c2, "a", refusal); code != 403 {
		t.Errorf("a refusal from a runner the run was not offered to: %d", code)
	}
	if code, env := f.result(t, c1, "a", refusal); code != 200 {
		t.Fatalf("refusal: %d %+v", code, env.Error)
	}
	if f.state(t, "a") != "failed" {
		t.Errorf("state after the refusal: %s", f.state(t, "a"))
	}
	if res := f.mustSync(t, "r2", c2, first("r2", 1)); len(res.Runs) != 0 {
		t.Errorf("a refused run was offered again: %v", ids(res.Runs))
	}
}

func TestEventsRefuseBadBatches(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.claimedBy(t, "r1", cred, "a")
	big := batch()
	for i := range maxBatch + 1 {
		big.Events = append(big.Events, batch(int64(i+1)).Events...)
	}
	for _, tc := range []struct {
		name string
		b    v1.EventBatch
	}{
		{"seq zero", batch(0)},
		{"negative seq", batch(-1)},
		{"too many", big},
	} {
		code, _, env := f.events(t, cred, "a", tc.b)
		if code != 400 || env.Error.Code != v1.CodeInvalid || env.Error.NextAction == "" {
			t.Errorf("%s: %d %+v", tc.name, code, env.Error)
		}
	}
}

func eventsAfter(runID string) db.EventsAfterParams {
	return db.EventsAfterParams{RunID: runID, Seq: 0, Limit: 10 * maxBatch}
}

// The hub refuses a run carrying a reserved grant at the door: it is never
// queued, so no runner is ever offered it.
func TestReservedGrantIsNeverQueued(t *testing.T) {
	f := newFixture(t)
	r := run("a", "s1")
	r.Grants = []v1.Grant{{Name: "ANTHROPIC_BASE_URL", Value: "https://attacker", As: v1.GrantEnv}}
	if err := f.store.EnqueueRun(context.Background(), r, f.clock.Now()); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_") {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := f.store.GetRun(context.Background(), "a"); err == nil {
		t.Error("the run was queued")
	}
}
