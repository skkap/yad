package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/skkap/yad/protocol/hubapi"
)

// The whole of DEV-36 end to end: a real daemon syncs with a real hub, and
// `yad hub runners` shows what that machine said about itself. Nothing here is
// hand-built — the health is what the runner computed and sent.
func TestE2EHubShowsRunnerHealth(t *testing.T) {
	m := newMachine(t, claudeE2E)
	d := m.daemon()

	var r hubapi.Runner
	eventually(t, "the hub has this runner's health", func() bool {
		runners, err := m.client().Runners(context.Background())
		if err != nil || len(runners) != 1 || runners[0].Health == nil {
			return false
		}
		r = runners[0]
		return true
	})
	h := r.Health
	if h.FreeCapacity.Total <= 0 || h.DiskFreeBytes <= 0 {
		t.Errorf("free capacity %+v, disk free %d:\n%s", h.FreeCapacity, h.DiskFreeBytes, d.out.String())
	}
	// The machine running the tests has a load; a machine yad does not ship
	// for would report 0, so this asserts only that it is not nonsense.
	if h.Load < 0 {
		t.Errorf("load = %v", h.Load)
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
