package hubapi

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// Health is a copy of v1.Health that keeps this API's promise of every
// dashboard field (decision 0047). A copy drifts: a field v1 gains and this
// one lacks is health the runner sent and the service never sees. So every
// field of v1.Health must be here under the same name, and HealthOf must carry
// each one across.
func TestHealthShowsEverythingTheRunnerSent(t *testing.T) {
	reset := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	sent := v1.Health{
		Load: 1.5, FreeCapacity: v1.Capacity{Total: 2, ByHarness: map[string]int{"claude": 1}}, DiskFreeBytes: 1 << 30,
		Harnesses:  []v1.HarnessHealth{{ID: "claude", Ready: true, Accounts: []v1.AccountReport{{Label: "work", LimitedUntil: &reset}}}},
		SpoolDepth: 3, OutboxDepth: 1, RecentErrors: []string{"2026-09-22T12:00:00Z WARN something"}, Draining: true,
	}
	for i := range reflect.TypeFor[v1.Health]().NumField() {
		f := reflect.TypeFor[v1.Health]().Field(i)
		if reflect.ValueOf(sent).Field(i).IsZero() {
			t.Fatalf("this test sets no value for v1.Health.%s, so it cannot tell whether HealthOf carries it", f.Name)
		}
	}
	a, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(HealthOf(sent))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("the runner sent\n%s\nand this API shows\n%s", a, b)
	}
}

// Where the runner left a dashboard field out, this API still answers it, as
// 0: it has always answered every one, and a service generated from it
// expects them.
func TestHealthAnswersTheDashboardFieldsTheRunnerLeftOut(t *testing.T) {
	b, err := json.Marshal(HealthOf(v1.Health{FreeCapacity: v1.Capacity{Total: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"load", "disk_free_bytes", "spool_depth", "outbox_depth"} {
		if string(got[name]) != "0" {
			t.Errorf("%s = %s, want 0: %s", name, got[name], b)
		}
	}
}
