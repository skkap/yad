package runner

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// The adapter is handed what a login check needs to name the account's home
// without printing it: the variable, the label, and yad commands on this
// runner's profile.
func TestTheRunsSpecCarriesWhatALoginCheckNeeds(t *testing.T) {
	e := newEnv(t)
	plantCredential(t, e.paths.Data, "work")
	var seen adapter.Spec
	ad := &fake.Adapter{ID: "claude", Next: func(s adapter.Spec) fake.Script {
		seen = s
		return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}}
	}}
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), ad)
	x.Paths = config.Paths{Profile: "side"}
	claimAndRun(t, l, x)

	if seen.HomeVar != "CLAUDE_CONFIG_DIR" || seen.Account != "work" {
		t.Errorf("spec HomeVar %q Account %q, want CLAUDE_CONFIG_DIR and work", seen.HomeVar, seen.Account)
	}
	shellwordtest.Check(t, seen.YadCommand("account", "list"), "yad", "--profile", "side", "account", "list")
}

// A run refused for want of a logged-in account tells the hub's reader what to
// type at the machine — on the runner's own profile, or it adds an account to
// a different runner. A runner whose data lives under YAD_DATA_DIR says the
// variable is needed without saying where it points: the message goes to a
// hub, and the path is under the owner's home (DEV-67).
func TestNoFreeAccountCommandRunsAsPrinted(t *testing.T) {
	data := t.TempDir()
	t.Setenv("YAD_DATA_DIR", data)
	relocated, err := config.Resolve("side")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		paths config.Paths
		env   map[string]string
		want  []string
	}{
		{config.Paths{Profile: config.DefaultProfile, Config: t.TempDir(), Data: t.TempDir()}, map[string]string{"YAD_DATA_DIR": ""}, []string{"yad", "account", "add", "claude", "work"}},
		{config.Paths{Profile: "side"}, map[string]string{"YAD_DATA_DIR": ""}, []string{"yad", "--profile", "side", "account", "add", "claude", "work"}},
		{relocated, map[string]string{"YAD_DATA_DIR": "<the runner's YAD_DATA_DIR>"}, []string{"yad", "--profile", "side", "account", "add", "claude", "work"}},
	} {
		err := &noFreeAccountError{paths: tc.paths, harness: "claude", accounts: []account.Account{
			{Harness: "claude", Label: "work", State: v1.AccountNeedsLogin},
		}}
		cmds := shellwordtest.Commands(err.Error(), "yad ")
		if len(cmds) != 1 {
			t.Fatalf("want one command in %q", err)
		}
		if strings.Contains(err.Error(), data) {
			t.Errorf("the message carries the runner's data directory, and goes to a hub: %s", err)
		}
		shellwordtest.CheckEnv(t, cmds[0], tc.env, tc.want...)
	}
}

// A runner already connected to a hub is told how to register it again — a
// command it pastes, so the hub's URL and the connection's name have to reach
// yad as they are, on the profile that was asked. The token is not known here
// and stays a placeholder; so does a URL whose query or fragment was redacted
// from the message, since pasting the redacted form would register against a
// different URL.
func TestConnectAgainCommandRunsAsPrinted(t *testing.T) {
	for _, tc := range []struct {
		name, url, wantURL string
	}{
		{"plain", "https://hub.example/yad/v1", "https://hub.example/yad/v1"},
		{"space", "https://hub.example/with space/v1", "https://hub.example/with space/v1"},
		{"dollar and backtick", "https://hub.example/$HOME/`id`/v1", "https://hub.example/$HOME/`id`/v1"},
		{"quote and semicolon", "https://hub.example/it's;echo injected/v1", "https://hub.example/it's;echo injected/v1"},
		{"ampersand", "https://hub.example/a&&echo injected/v1", "https://hub.example/a&&echo injected/v1"},
		{"redacted query", "https://hub.example/v1?sig=abc&x=1", "<the same URL>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := config.Paths{Profile: "work", Config: t.TempDir(), Data: t.TempDir()}
			cfg := config.Default()
			cfg.Connections = []config.Connection{{Name: "first", URL: tc.url}}
			if err := config.Save(p, cfg); err != nil {
				t.Fatal(err)
			}
			_, _, _, err := Connect(context.Background(), p, tc.url, "t", "second")
			if err == nil {
				t.Fatal("a second connection to the same hub was accepted")
			}
			cmds := shellwordtest.Commands(err.Error(), "yad ")
			if len(cmds) != 1 {
				t.Fatalf("want one command in %q", err)
			}
			shellwordtest.Check(t, cmds[0], "yad", "--profile", "work", "connect", tc.wantURL, "--name", "first", "--token", "<new token>")
		})
	}
}

// `yad connect` with no token names the two commands that fix it: the hub's,
// with what only its operator knows as placeholders, and this one again as it
// runs here, with the URL and name it was given.
func TestConnectWithNoTokenNamesCommandsThatRun(t *testing.T) {
	p := config.Paths{Profile: "work", Config: t.TempDir(), Data: t.TempDir()}
	const url = "https://hub.example/it's $HOME/v1"
	_, _, _, err := Connect(context.Background(), p, url, "", "home")
	if err == nil {
		t.Fatal("connected with no token")
	}
	cmds := shellwordtest.Commands(err.Error(), "yad ")
	if len(cmds) != 2 {
		t.Fatalf("want two commands in %q", err)
	}
	shellwordtest.Check(t, cmds[0], "yad", "--profile", "<hub profile>", "hub", "token", "create", "--db", "<the database yad hub serve was given>")
	shellwordtest.Check(t, cmds[1], "yad", "--profile", "work", "connect", url, "--name", "home", "--token", "-")
}
