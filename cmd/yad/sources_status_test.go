package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/protocol/hubapi"
)

// `yad status` lists, under each connection, the repositories its hub asked
// about and what the probe found: the class the hub was sent, and a line for
// one not checked yet (decision 0088).
func TestStatusShowsTheRepositoriesAHubAskedAbout(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	checked := now.Add(-2 * time.Minute)
	var b bytes.Buffer
	printStatus(&b, config.Paths{}, control.Status{Started: now, Connections: []control.Connection{{
		Name: "zumino", URL: "https://zumino.cc/yad/v1", State: "syncing",
		Sources: []control.SourceProbe{
			{URL: "https://github.com/acme/api.git", Reachable: true, Checked: &checked},
			{URL: "git@github.com:acme/web.git", Ref: "main", Error: "auth", Checked: &checked},
			{URL: "https://github.com/acme/new\x1b[2J.git"},
		},
	}}}, now)
	out := b.String()
	for _, want := range []string{
		"repository https://github.com/acme/api.git: reachable, checked 2m0s ago",
		"repository git@github.com:acme/web.git main: unreachable (auth), checked 2m0s ago",
		".git: not checked yet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("a hub's URL reached the terminal uncleaned:\n%q", out)
	}
}

// `yad hub runners` counts what each runner said about the hub's
// repositories and names each it cannot reach, which is what routes runs
// away from it.
func TestHubRunnersShowWhatEachRunnerCanReach(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	printRunners(&b, []hubapi.Runner{{RunnerID: "r1", Name: "ashikaga", LastSyncAt: &now, Health: &hubapi.Health{},
		SourceReach: []hubapi.SourceReach{
			{URL: "https://github.com/acme/api.git", Reachable: true, CheckedAt: now},
			{URL: "https://github.com/other/web.git", Error: "not_found", CheckedAt: now.Add(-time.Minute)},
		},
	}}, now)
	out := b.String()
	for _, want := range []string{"repositories: 1 of 2 reachable", "https://github.com/other/web.git — not_found, checked 1m0s ago"} {
		if !strings.Contains(out, want) {
			t.Errorf("hub runners does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "acme/api.git") {
		t.Errorf("a reachable repository was listed one by one:\n%s", out)
	}
}
