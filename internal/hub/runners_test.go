package hub

import (
	"net/http"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"
)

// richHealth is a sync carrying the whole of ARCHITECTURE §2's health block,
// as a runner that has been up for a while sends it.
func richHealth(id string) v1.SyncRequest {
	r := first(id, 2)
	reset := time.Date(2026, 9, 19, 17, 0, 0, 0, time.UTC)
	r.Health = v1.Health{
		Load:          1.75,
		FreeCapacity:  v1.Capacity{Total: 2, ByHarness: map[string]int{"claude": 2}},
		DiskFreeBytes: 128 << 30,
		SpoolDepth:    3,
		OutboxDepth:   1,
		Harnesses: []v1.HarnessHealth{{
			ID:    "claude",
			Ready: true,
			Accounts: []v1.AccountReport{
				{Label: "personal", State: v1.AccountFree, Windows: []v1.AccountWindow{{Name: "five_hour", UsedPercent: 42, ResetsAt: &reset}}},
				{Label: "work", State: v1.AccountLimited, LimitedUntil: &reset},
			},
		}, {
			// A harness with no accounts runs on its own login. Health has to
			// render it, and it is not an error.
			ID: "codex", Ready: true,
		}},
		RecentErrors: []string{"2026-09-19T11:59:00Z WARN a workdir could not be reclaimed"},
	}
	return r
}

// The hub receives health on every sync; this is the read that makes it worth
// receiving. Every field of §2's block comes back as the runner sent it.
func TestRunnersShowTheHealthEachRunnerLastSent(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	f.mustSync(t, "r1", cred, richHealth("r1"))
	// A second runner that registered and has never synced: it is in the
	// list, and it has no health rather than an empty one.
	f.register(t, "r2")

	var list hubapi.RunnerList
	if code, e := f.api(t, "GET", "/runners", tok, nil, &list); code != http.StatusOK {
		t.Fatalf("list: %d %+v", code, e)
	}
	if len(list.Runners) != 2 {
		t.Fatalf("runners = %+v, want two", list.Runners)
	}
	// The one still syncing comes first.
	got, never := list.Runners[0], list.Runners[1]
	if got.RunnerID != "r1" || never.RunnerID != "r2" {
		t.Fatalf("order = %s, %s; want the runner that synced first", got.RunnerID, never.RunnerID)
	}
	if never.Health != nil || never.LastSyncAt != nil {
		t.Errorf("a runner that never synced has health %+v", never.Health)
	}
	if got.LastSyncAt == nil || got.Health == nil {
		t.Fatalf("r1 = %+v", got)
	}
	h := got.Health
	if h.Load != 1.75 || h.FreeCapacity.Total != 2 || h.FreeCapacity.ByHarness["claude"] != 2 {
		t.Errorf("load %v, free %+v", h.Load, h.FreeCapacity)
	}
	if h.DiskFreeBytes != 128<<30 || h.SpoolDepth != 3 || h.OutboxDepth != 1 || h.Draining {
		t.Errorf("disk %d, spool %d, outbox %d, draining %v", h.DiskFreeBytes, h.SpoolDepth, h.OutboxDepth, h.Draining)
	}
	if len(h.RecentErrors) != 1 {
		t.Errorf("recent_errors = %q", h.RecentErrors)
	}
	if len(h.Harnesses) != 2 {
		t.Fatalf("harnesses = %+v", h.Harnesses)
	}
	claude, codex := h.Harnesses[0], h.Harnesses[1]
	if !claude.Ready || len(claude.Accounts) != 2 {
		t.Fatalf("claude = %+v", claude)
	}
	if a := claude.Accounts[0]; a.Label != "personal" || a.State != v1.AccountFree || len(a.Windows) != 1 ||
		a.Windows[0].Name != "five_hour" || a.Windows[0].UsedPercent != 42 || a.Windows[0].ResetsAt == nil {
		t.Errorf("personal = %+v", a)
	}
	if a := claude.Accounts[0]; a.LimitedUntil != nil {
		t.Errorf("a free account carries limited_until = %v", a.LimitedUntil)
	}
	if a := claude.Accounts[1]; a.State != v1.AccountLimited || a.LimitedUntil == nil || len(a.Windows) != 0 {
		t.Errorf("work = %+v", a)
	}
	// The case DEV-26 settled: no accounts is a state, not a gap.
	if !codex.Ready || len(codex.Accounts) != 0 {
		t.Errorf("codex = %+v, want ready with no accounts", codex)
	}

	var one hubapi.Runner
	if code, e := f.api(t, "GET", "/runners/r1", tok, nil, &one); code != http.StatusOK || one.Health == nil || one.Health.Load != 1.75 {
		t.Fatalf("get: %d %+v %+v", code, e, one)
	}
	if code, e := f.api(t, "GET", "/runners/nope", tok, nil, nil); code != http.StatusNotFound {
		t.Errorf("an unknown runner: %d %+v", code, e)
	}
}

// The fleet is the operator's business, not a runner's: a runner credential
// opens no door on the service API.
func TestRunnersNeedAnAdminToken(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.mustSync(t, "r1", cred, richHealth("r1"))
	for _, path := range []string{"/runners", "/runners/r1"} {
		for _, tok := range []string{"", cred} {
			if code, _ := f.api(t, "GET", path, tok, nil, nil); code != http.StatusUnauthorized {
				t.Errorf("GET %s with %q: %d, want 401", path, tok, code)
			}
		}
	}
}

// An empty fleet is an answer, not a 404 — and the list is a list, never null,
// so a service decoding it can iterate without a nil check.
func TestAnEmptyFleetIsAnEmptyList(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	var list hubapi.RunnerList
	if code, e := f.api(t, "GET", "/runners", tok, nil, &list); code != http.StatusOK || list.Runners == nil || len(list.Runners) != 0 {
		t.Fatalf("list: %d %+v %+v", code, e, list.Runners)
	}
}
