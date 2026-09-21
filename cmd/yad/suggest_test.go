package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
	"github.com/skkap/yad/internal/upgrade"
)

// Every command below is checked the way an owner meets it: pasted into a real
// sh, whose argv is compared with what the command was meant to run. A value
// holding a space, a $, a backtick, a quote or a ; has to arrive as itself.

// onlyCommand is the one backticked command in msg that starts with prefix.
func onlyCommand(t *testing.T, msg, prefix string) string {
	t.Helper()
	found := shellwordtest.Commands(msg, prefix)
	if len(found) != 1 {
		t.Fatalf("want one `%s…` command in\n  %s\nfound %q", prefix, msg, found)
	}
	return found[0]
}

// A release named as an argument is refused with the flag that means it — and
// the argument is whatever the owner typed, so the command has to survive it.
func TestUpgradeArgumentRefusalRunsAsPrinted(t *testing.T) {
	for _, tag := range shellwordtest.Hostile {
		if tag == "" {
			continue // an empty argument is not one parseInterleaved keeps
		}
		t.Run(tag, func(t *testing.T) {
			code, _, errs := yad(t, "upgrade", tag)
			if code != 1 {
				t.Fatalf("exit %d: %s", code, errs)
			}
			shellwordtest.Check(t, onlyCommand(t, errs, "yad upgrade"), "yad", "upgrade", "--tag", tag)
		})
	}
}

// The tag in checkLine comes from --tag or from the release source — a name
// the network supplied — and every command offered has to install that tag.
func TestCheckLineCommandsRunAsPrinted(t *testing.T) {
	for _, tag := range shellwordtest.Hostile {
		for _, state := range []upgrade.State{upgrade.Behind, upgrade.Ahead, upgrade.Unstamped, upgrade.UnreadableTag} {
			line := checkLine(state, tag, true)
			for _, cmd := range shellwordtest.Commands(line, "yad upgrade") {
				shellwordtest.Check(t, cmd, "yad", "upgrade", "--tag", tag)
			}
		}
	}
}

func TestRestartAdviceRunsAsPrinted(t *testing.T) {
	for _, tc := range []struct {
		profile         string
		service, daemon []string
	}{
		{"work", []string{"yad", "service", "install", "--profile", "work"}, []string{"yad", "--profile", "work", "daemon", "restart"}},
		{config.DefaultProfile, []string{"yad", "service", "install"}, []string{"yad", "daemon", "restart"}},
	} {
		line := restartAdvice(tc.profile)
		shellwordtest.Check(t, onlyCommand(t, line, "yad service"), tc.service...)
		shellwordtest.Check(t, onlyCommand(t, line, strings.Join(tc.daemon[:len(tc.daemon)-1], " ")), tc.daemon...)
	}
}

// An account needing login is listed with the command that logs it in — for
// the profile the listing was of. Pasted bare under another profile, it would
// add an account to the default runner instead.
func TestAccountListLoginCommandRunsAsPrinted(t *testing.T) {
	p := accountEnv(t)
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"work"}}}
	if err := config.Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := account.Ensure(p.Data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if err := recordState(context.Background(), p, "claude", "work", v1.AccountNeedsLogin); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		global []string
		want   []string
	}{
		{nil, []string{"yad", "account", "add", "claude", "work"}},
		{[]string{"--profile", "side"}, []string{"yad", "--profile", "side", "account", "add", "claude", "work"}},
	} {
		code, out, errs := yadIn(t, append(tc.global, "account", "list")...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errs)
		}
		shellwordtest.Check(t, onlyCommand(t, out, "yad "), tc.want...)
	}
}

// `yad hub submit` and `yad hub cancel` point at `yad hub watch` for the run —
// on the hub, with the token file, the owner just used. A bare one would watch
// the default hub with the default token and answer "no such run".
func TestHubWatchCommandCarriesTheHub(t *testing.T) {
	r := newHubRig(t)
	service := os.Getenv("YAD_HUB_URL")
	t.Setenv("YAD_HUB_URL", "")
	// A token file whose path needs quoting, holding the rig's admin token.
	tok, err := config.ReadSecret(filepath.Join(r.p.config, "hub-admin-token"))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "it's a $token")
	if err := config.WriteSecret(file, tok); err != nil {
		t.Fatal(err)
	}
	flags := []string{"--hub", service, "--token-file", file}

	code, out, errs := r.p.yad("", append(append([]string{"hub", "submit", "--harness", "claude", "--model", "opus"}, flags...), "hi")...)
	if code != 0 {
		t.Fatalf("submit: exit %d: %s", code, errs)
	}
	id := strings.TrimSpace(out)
	want := append(append([]string{"yad", "hub", "watch"}, flags...), id)
	shellwordtest.Check(t, onlyCommand(t, errs, "yad hub watch"), want...)

	// Held by a runner, so the cancel is queued and the run is still to watch.
	if _, err := r.s.DB.ExecContext(context.Background(), "UPDATE runs SET state = 'running' WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	code, out, errs = r.p.yad("", append(append([]string{"hub", "cancel"}, flags...), id)...)
	if code != 0 {
		t.Fatalf("cancel: exit %d: %s", code, errs)
	}
	shellwordtest.Check(t, onlyCommand(t, out, "yad hub watch"), want...)
}

// An admin token already saved is revoked on the hub it came from, so the
// command offered lists the tokens in the database this create was pointed at.
func TestAdminTokenListCommandCarriesTheDatabase(t *testing.T) {
	p := newProfile(t)
	db := filepath.Join(t.TempDir(), "it's a hub.db")
	if code, _, errs := p.yad("", "hub", "admin-token", "create", "--db", db); code != 0 {
		t.Fatalf("create: exit %d: %s", code, errs)
	}
	code, _, errs := p.yad("", "hub", "admin-token", "create", "--db", db, "--name", "other")
	if code == 0 {
		t.Fatal("a second create over a saved token was accepted")
	}
	shellwordtest.Check(t, onlyCommand(t, errs, "yad hub admin-token"), "yad", "hub", "admin-token", "list", "--db", db)
}
