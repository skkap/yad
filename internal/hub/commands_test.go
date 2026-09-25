package hub

import (
	"net/http"
	"strings"
	"testing"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// Every command a hub's answer names runs as printed (AGENTS.md): pasted into
// a shell it reaches yad as the argv shown here, with what the hub cannot know
// — the runner's profile, the caller's profile and URL for this hub, the
// hub's own profile and database — as placeholders that are refused unfilled,
// and what it does know, the ids and names from the request, quoted whatever
// they hold.
func TestTheCommandsAHubNamesRunAsPrinted(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	old := f.register(t, "old")
	f.mustSync(t, "old", old, stale("old", 1))
	f.enqueue(t, run("queued run", "s1"))
	const hostile = `it's $HOME; echo "x"`
	caller := []string{"yad", "--profile", "<profile holding the admin token>", "hub"}
	for _, tc := range []struct {
		name         string
		method, path string
		token        string
		body         any
		prefix       string
		want         []string
	}{
		{"a login on a runner that cannot take one", "POST", "/runners/old/logins", tok, hubapi.LoginRequest{Harness: hostile, Account: "work"},
			"yad ", []string{"yad", "--profile", "<runner profile>", "account", "add", hostile, "work"}},
		{"a default login on a runner that cannot take one", "POST", "/runners/old/logins", tok, hubapi.LoginRequest{Harness: "claude"},
			"yad ", []string{"yad", "--profile", "<runner profile>", "doctor"}},
		{"a login naming no label", "POST", "/runners/old/logins", tok, hubapi.LoginRequest{Harness: "claude", Account: "Not A Label"},
			"yad ", []string{"yad", "--profile", "<runner profile>", "account", "list"}},
		{"a login on a runner the hub does not know", "POST", "/runners/nope/logins", tok, hubapi.LoginRequest{Harness: "claude"},
			"yad ", append(caller, "runners", "--hub", "<hub url>")},
		{"a runner the hub does not know", "GET", "/runners/nope", tok, nil,
			"yad ", append(caller, "runners", "--hub", "<hub url>")},
		{"a drain of a runner the hub does not know", "POST", "/runners/nope/drain", tok, nil,
			"yad ", append(caller, "runners", "--hub", "<hub url>")},
		{"an interrupt of a run no runner started", "POST", "/runs/queued run/interrupt", tok, nil,
			"yad ", append(caller, "cancel", "--hub", "<hub url>", "queued run")},
		{"no admin token", "GET", "/runners", "", nil,
			"yad ", []string{"yad", "--profile", "<hub profile>", "hub", "admin-token", "create", "--db", "<the database yad hub serve was given>"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, e := f.api(t, tc.method, strings.ReplaceAll(tc.path, " ", "%20"), tc.token, tc.body, nil)
			if code < http.StatusBadRequest {
				t.Fatalf("answered %d", code)
			}
			cmds := shellwordtest.Commands(e.NextAction, tc.prefix)
			if len(cmds) != 1 {
				t.Fatalf("want one command in %q", e.NextAction)
			}
			shellwordtest.Check(t, cmds[0], tc.want...)
		})
	}
	// An interrupt refused for a runner without the feature, by the same
	// builder, for a run id the caller chose.
	_, alt := controlFeature(v1.ControlInterrupt, hostile)
	shellwordtest.Check(t, shellwordtest.Commands(alt, "yad ")[0], append(caller, "cancel", "--hub", "<hub url>", hostile)...)
}
