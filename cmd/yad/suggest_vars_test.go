package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
	"github.com/skkap/yad/internal/store"
)

// A command yad prints acts on the runner the message came from only if it
// carries what chose that runner: the profile, and the directory variables
// the profile was resolved with. Pasted into a shell with neither, each of
// these acts on the default runner — or on an empty one — and answers as if
// that were the runner asked about.

// dirsEnv is what every command printed under the test's profile has to set:
// the directory overrides the test gave, and no XDG base beside them.
func dirsEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"YAD_CONFIG_DIR":  os.Getenv("YAD_CONFIG_DIR"),
		"YAD_DATA_DIR":    os.Getenv("YAD_DATA_DIR"),
		"XDG_CONFIG_HOME": "",
		"XDG_DATA_HOME":   "",
	}
}

func TestProfileCommandsCarryTheProfileAndItsDirectories(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"daemon status with no daemon", []string{"daemon", "status"}, []string{"yad", "--profile", "side", "daemon", "start"}},
		{"status with no daemon", []string{"status"}, []string{"yad", "--profile", "side", "daemon", "start"}},
		{"sessions close of a session it lacks", []string{"sessions", "close", "nope"}, []string{"yad", "--profile", "side", "sessions"}},
		{"account list with no accounts", []string{"account", "list"}, []string{"yad", "--profile", "side", "account", "add", "<harness>", "<label>"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accountEnv(t)
			_, out, errs := yadIn(t, append([]string{"--profile", "side"}, tc.args...)...)
			shellwordtest.CheckEnv(t, onlyCommand(t, out+errs, strings.Join(tc.want[:3], " ")), dirsEnv(t), tc.want...)
		})
	}
}

// A connection whose credential is missing is repaired by connecting again
// under the same name, on the same profile — the error says so as a command.
func TestMissingCredentialCommandCarriesTheConnection(t *testing.T) {
	p := accountEnv(t)
	cfg := config.Default()
	cfg.Connections = []config.Connection{{Name: "home", URL: "https://hub.example/yad/v1"}}
	if err := config.Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	_, out, errs := yadIn(t, "--profile", "side", "daemon", "restart")
	shellwordtest.CheckEnv(t, onlyCommand(t, out+errs, "yad --profile side connect"), dirsEnv(t),
		"yad", "--profile", "side", "connect", "<hub url>", "--name", "home", "--token", "<new token>")
}

// A state database the daemon has not migrated yet is brought up to date by
// restarting that profile's daemon, not the default one.
func TestSchemaBehindCommandCarriesTheProfile(t *testing.T) {
	p := accountEnv(t)
	s, err := store.Open(context.Background(), p.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, out, errs := yadIn(t, "--profile", "side", "sessions")
	shellwordtest.CheckEnv(t, onlyCommand(t, out+errs, "yad --profile side daemon"), dirsEnv(t),
		"yad", "--profile", "side", "daemon", "restart")
}

// `yad upgrade` fetches from YAD_REPO when it is set — a fork's install — and
// a command offered without it would fetch upstream's release over the fork.
func TestUpgradeCommandsCarryTheFork(t *testing.T) {
	t.Setenv("YAD_REPO", "someone/yad fork")
	_, _, errs := yad(t, "upgrade", "v1.2.3")
	shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad upgrade"), map[string]string{"YAD_REPO": "someone/yad fork"},
		"yad", "upgrade", "--tag", "v1.2.3")
}

// A hub named by $YAD_HUB_URL is carried as --hub: the shell the watch is
// pasted into need not have the variable.
func TestHubWatchCommandCarriesAHubFromTheEnvironment(t *testing.T) {
	r := newHubRig(t)
	service := os.Getenv("YAD_HUB_URL")
	code, out, errs := r.p.yad("", "hub", "submit", "--harness", "claude", "--model", "opus", "hi")
	if code != 0 {
		t.Fatalf("submit: exit %d: %s", code, errs)
	}
	shellwordtest.Check(t, onlyCommand(t, errs, "yad hub watch"), "yad", "hub", "watch", "--hub", service, strings.TrimSpace(out))
}
