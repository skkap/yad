package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"
)

// What an operator sees: every part of a runner's health, including the two
// states that are answers rather than faults — a harness with no accounts at
// all, and a runner that has never synced.
func TestHubRunnersPrintsEveryPartOfHealth(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	synced := now.Add(-20 * time.Second)
	reset := now.Add(2 * time.Hour)
	var buf bytes.Buffer
	printRunners(&buf, []hubapi.Runner{{
		RunnerID: "r1", Name: "laptop", LastSyncAt: &synced,
		Health: &v1.Health{
			Load:          1.75,
			FreeCapacity:  v1.Capacity{Total: 2, ByHarness: map[string]int{"claude": 1}},
			DiskFreeBytes: 128 << 30,
			SpoolDepth:    3, OutboxDepth: 1,
			Harnesses: []v1.HarnessHealth{
				{ID: "claude", Ready: false, Accounts: []v1.AccountReport{
					{Label: "work", State: v1.AccountLimited, LimitedUntil: &reset, Windows: []v1.AccountWindow{
						{Name: "five_hour", UsedPercent: 100}, {Name: "seven_day", UsedPercent: 61},
					}},
					{Label: "spare", State: v1.AccountNeedsLogin},
				}},
				{ID: "codex", Ready: true},
			},
			RecentErrors: []string{"2026-09-19T11:59:00Z WARN a workdir could not be reclaimed"},
		},
	}, {
		RunnerID: "r2", Name: "builder",
	}}, now)
	out := buf.String()
	for _, want := range []string{
		"laptop (r1)", "synced 20s ago",
		"load 1.75", "2 free", "disk 128.0 GiB", "spool 3", "outbox 1",
		"claude", "not ready, 1 free",
		"work limited until", "five_hour 100%", // the window nearest its limit, not the other one
		"spare needs_login",
		"codex", "no accounts configured",
		"a workdir could not be reclaimed",
		"builder (r2)", "never synced", "no health yet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "seven_day") {
		t.Errorf("the summary names a window that is not the fullest:\n%s", out)
	}
}

// A runner's health crossed a network from a machine the hub does not own.
// Anything printed from it is cleaned, or a label could draw rows of its own
// in the operator's terminal — or move the cursor.
func TestHubRunnersCleansWhatARunnerSent(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	printRunners(&buf, []hubapi.Runner{{
		RunnerID: "r\x1b[2J1", Name: "lap\ntop",
		Health: &v1.Health{
			// State is a wire string too. The first version of this test set
			// it to v1.AccountFree — the one value that cannot expose an
			// uncleaned print — and so certified the field it was written to
			// check.
			Harnesses:    []v1.HarnessHealth{{ID: "cla\tude", Accounts: []v1.AccountReport{{Label: "wo\x07rk", State: v1.AccountState("free\x1b[2J\x1b[H")}}}},
			RecentErrors: []string{"2026-09-19T11:59:00Z WARN a hub\nsaid\x1b[31m so"},
		},
	}}, now)
	for _, bad := range []string{"\x1b", "\x07", "lap\ntop", "said\x1b"} {
		if strings.Contains(buf.String(), bad) {
			t.Errorf("%q survived into the terminal:\n%q", bad, buf.String())
		}
	}
	// Cleaned, not dropped: clean() replaces a control byte with a marker
	// rather than deleting it, so the operator still sees which runner it was
	// — and a state carrying an escape is no longer one of the closed set, so
	// it reads as news rather than as "free".
	if !strings.Contains(buf.String(), "top") || !strings.Contains(buf.String(), "rk state free") {
		t.Errorf("cleaning lost the content:\n%s", buf.String())
	}
}

// Every value of the closed set reads as its own word, an absent state says so
// rather than leaving a blank that would pass for free, and a state this
// binary has never heard of is shown as news — cleaned, since only a hub that
// is newer, hostile or broken can produce one.
func TestHubRunnersRendersEveryAccountState(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		state  v1.AccountState
		want   string
		absent string
	}{
		{name: "free", state: v1.AccountFree, want: "work free"},
		{name: "limited", state: v1.AccountLimited, want: "work limited"},
		{name: "needs login", state: v1.AccountNeedsLogin, want: "work needs_login"},
		{name: "absent, from a runner older than the field", state: "", want: "work state unknown"},
		{name: "one this binary does not know", state: v1.AccountState("re\x1bsting"), want: "work state re", absent: "\x1b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			printRunners(&buf, []hubapi.Runner{{RunnerID: "r1", Name: "laptop", LastSyncAt: &now, Health: &v1.Health{
				Harnesses: []v1.HarnessHealth{{ID: "claude", Accounts: []v1.AccountReport{{Label: "work", State: tc.state}}}},
			}}}, now)
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("no %q in:\n%s", tc.want, buf.String())
			}
			if tc.absent != "" && strings.Contains(buf.String(), tc.absent) {
				t.Errorf("%q survived into the terminal:\n%q", tc.absent, buf.String())
			}
		})
	}
	// The closed set is v1's, so a state added there without a word here
	// would fall to the default and read as news rather than as itself.
	for _, s := range v1.AccountStates() {
		if got := accountStateWord(s); got != string(s) {
			t.Errorf("accountStateWord(%q) = %q; v1 has a state this listing does not name", s, got)
		}
	}
}

// Nothing registered is an answer with the next action, not an empty screen.
func TestHubRunnersSaysWhenThereAreNone(t *testing.T) {
	var buf bytes.Buffer
	printRunners(&buf, nil, time.Now())
	if !strings.Contains(buf.String(), "yad hub token create") {
		t.Errorf("no next action for an empty fleet: %q", buf.String())
	}
}
