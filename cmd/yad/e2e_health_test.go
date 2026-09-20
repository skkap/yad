package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"
)

// The whole of DEV-36 end to end: a real daemon syncs with a real hub, and
// `yad hub runners` shows what that machine said about itself. Nothing here is
// hand-built — the health is what the runner computed and sent.
func TestE2EHubShowsRunnerHealth(t *testing.T) {
	m := newMachine(t, claudeE2E)
	d := m.daemon()

	// Health arrives in two stages, and waiting for the first while asserting
	// about the second is what made this test flaky.
	//
	// A sync made before the reporter's first flush carries no free capacity
	// at all: SyncOnce leaves the reservation empty unless mayClaim, and
	// mayClaim is false until ClaimAfter closes, which is what the runner
	// owes a previous process going out before new work comes in (decision
	// 0030). It still syncs meanwhile so leases renew, so the hub really can
	// hold a health report saying Total: 0 on a runner whose banner says
	// capacity 4. That is the design and not a defect — a hub offers such a
	// runner nothing, which is what the runner wants until it is claiming.
	//
	// So each wait names the state it is waiting for, and neither asserts
	// about a state it did not wait for.
	r := waitForHealth(t, m, d, "any health at all", func(*v1.Health) bool { return true })
	h := r.Health
	if h.DiskFreeBytes <= 0 {
		t.Errorf("disk free %d:\n%s", h.DiskFreeBytes, d.out.String())
	}
	// The machine running the tests has a load; a machine yad does not ship
	// for would report 0, so this asserts only that it is not nonsense.
	if h.Load < 0 {
		t.Errorf("load = %v", h.Load)
	}
	// And now the later state, waited for on its own terms rather than
	// assumed to have arrived with the first.
	claiming := waitForHealth(t, m, d, "a sync made once the runner is claiming, which is the first that advertises capacity",
		func(h *v1.Health) bool { return h.FreeCapacity.Total > 0 })
	// The wait's own condition, restated. It cannot fire — waitForHealth
	// returns only when it holds, and reports on its timeout instead — and it
	// is here so that what this test asserts about capacity is visible where a
	// reader looks for it rather than only inside a predicate above.
	if claiming.Health.FreeCapacity.Total <= 0 {
		t.Errorf("free capacity %+v", claiming.Health.FreeCapacity)
	}
	if len(h.Harnesses) == 0 {
		t.Fatalf("no harness health:\n%s", d.out.String())
	}
	// The e2e machine configures no accounts, so this is the case DEV-26
	// settled, proved end to end: the harness runs on its own login, reports
	// zero accounts, is ready, and nothing about it is an error.
	for _, hh := range h.Harnesses {
		if !hh.Ready || len(hh.Accounts) != 0 {
			t.Errorf("harness %s = %+v, want ready with no accounts", hh.ID, hh)
		}
	}

	// And the operator's view of it, through the CLI that reads it.
	out := m.ok("hub", "runners", "--hub", m.service)
	for _, want := range []string{r.RunnerID, "load ", "free", "disk ", "no accounts configured"} {
		if !strings.Contains(out, want) {
			t.Errorf("`yad hub runners` lacks %q:\n%s", want, out)
		}
	}
	var list hubapi.RunnerList
	if err := json.Unmarshal([]byte(m.ok("hub", "runners", "--hub", m.service, "--json")), &list); err != nil {
		t.Fatalf("--json: %v", err)
	}
	if len(list.Runners) != 1 || list.Runners[0].Health == nil {
		t.Errorf("--json = %+v", list.Runners)
	}
}

// waitForHealth polls the hub for this runner until its stored health
// satisfies want, and returns the runner as the hub then holds it.
//
// It exists rather than eventually() because a timeout here has to say what
// it saw. "Timed out waiting until the runner is claiming" sends the next
// person to the wrong place; the health reports that did arrive say whether
// the runner never claimed, never synced, or synced with something
// unexpected in it.
func waitForHealth(t *testing.T, m *machine, d *daemon, what string, want func(*v1.Health) bool) hubapi.Runner {
	t.Helper()
	// Counted by free capacity rather than logged per poll. The first version
	// of this kept one line per changed report and printed a hundred and
	// sixty of them, because disk free moves between every poll on a live
	// machine — accurate, and it buried the single fact the reader needs,
	// which is which capacities were ever advertised.
	freeSeen := map[int]int{}
	var last string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		runners, err := m.client().Runners(context.Background())
		if err != nil || len(runners) != 1 || runners[0].Health == nil {
			continue
		}
		r := runners[0]
		if want(r.Health) {
			return r
		}
		freeSeen[r.Health.FreeCapacity.Total]++
		last = fmt.Sprintf("free=%d disk=%d harnesses=%d recent_errors=%d", r.Health.FreeCapacity.Total,
			r.Health.DiskFreeBytes, len(r.Health.Harnesses), len(r.Health.RecentErrors))
	}
	counts := make([]string, 0, len(freeSeen))
	for _, total := range slices.Sorted(maps.Keys(freeSeen)) {
		counts = append(counts, fmt.Sprintf("%d free ×%d", total, freeSeen[total]))
	}
	if last == "" {
		last = "none — the hub never held health for this runner"
	}
	t.Fatalf("no sync in 30s carried %s\nfree capacity advertised: %s\nlast health: %s\ndaemon:\n%s",
		what, strings.Join(counts, ", "), last, d.out.String())
	return hubapi.Runner{}
}
