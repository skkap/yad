package capability

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// A home whose runs went silent is a warning with the time it is tried again
// and the way to try it sooner, and the harness is not ready only when no
// home of it is left that can take a run. Never an error: an error would have
// the runner refuse the run outright, and this ends by itself (decision 0077).
func TestASilentHomeIsAWarningAndNotReadyOnlyWhenNoneIsLeft(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	until := now.Add(15 * time.Minute)
	withAccounts := config.Default()
	withAccounts.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"work", "spare"}}}
	silentWork := account.Account{Harness: "claude", Label: "work", State: v1.AccountFree, SilentUntil: &until, SilentRuns: 3}
	for _, tc := range []struct {
		name     string
		cfg      config.Config
		accounts []account.Account
		silences account.Silences
		notReady bool
		says     []string
	}{
		{"its own login, silent", config.Default(), nil,
			account.Silences{{Harness: "claude"}: {Harness: "claude", Runs: 3, Until: until}}, true,
			[]string{"its own login has said nothing in its last 3 runs", "until 2026-10-11T12:15:00Z", "account add"}},
		{"its own login, past its cool-down", config.Default(), nil,
			account.Silences{{Harness: "claude"}: {Harness: "claude", Runs: 3, Until: now}}, false, nil},
		{"its own login, two silent runs", config.Default(), nil,
			account.Silences{{Harness: "claude"}: {Harness: "claude", Runs: 2}}, false, nil},
		{"one account silent and one free", withAccounts,
			[]account.Account{silentWork, {Harness: "claude", Label: "spare", State: v1.AccountFree}}, nil, false,
			[]string{`account "work" has said nothing in its last 3 runs`}},
		{"one account silent, the other at a limit", withAccounts,
			[]account.Account{silentWork, {Harness: "claude", Label: "spare", State: v1.AccountLimited}}, nil, true,
			[]string{`account "work"`}},
		{"its own login's record, once it has accounts", withAccounts,
			[]account.Account{{Harness: "claude", Label: "work", State: v1.AccountFree}},
			account.Silences{{Harness: "claude"}: {Harness: "claude", Runs: 3, Until: until}}, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found := readyClaude()
			got := SilentHomes(found, tc.cfg, tc.accounts, tc.silences, now, config.Paths{Profile: "work"}.RemoteCommand)
			if got["claude"] != tc.notReady {
				t.Errorf("not ready = %v, want %v", got["claude"], tc.notReady)
			}
			if !found[0].Ready() {
				t.Errorf("the harness was given an error: %+v", found[0])
			}
			w := strings.Join(found[0].Warnings, "\n")
			if len(tc.says) == 0 && w != "" {
				t.Errorf("warnings %q", w)
			}
			for _, s := range tc.says {
				if !strings.Contains(w, s) {
					t.Errorf("warnings %q lack %q", w, s)
				}
			}
			if len(tc.says) > 0 {
				cmds := shellwordtest.Commands(found[0].Warnings[0], "yad ")
				if len(cmds) == 0 {
					t.Fatalf("no command in %q", found[0].Warnings[0])
				}
				shellwordtest.Check(t, cmds[0], "yad", "--profile", "work", "daemon", "restart")
			}
		})
	}
}

// The account report a hub reads carries silent_until while the account is
// left alone, and the account's state stays the protocol's own.
func TestTheDocumentCarriesAnAccountsSilentUntil(t *testing.T) {
	until := time.Date(2026, 10, 11, 12, 15, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"work"}}}
	found := []harness.Detected{{Harness: harness.Harness{ID: "claude", Kind: harness.FirstClass}, Present: true}}
	reps := Harnesses(found, cfg, []account.Account{{Harness: "claude", Label: "work", State: v1.AccountFree, SilentUntil: &until, SilentRuns: 3}})
	if len(reps) != 1 || len(reps[0].Accounts) != 1 {
		t.Fatalf("reports = %+v", reps)
	}
	a := reps[0].Accounts[0]
	if a.State != v1.AccountFree || a.SilentUntil == nil || !a.SilentUntil.Equal(until) {
		t.Errorf("account = %+v", a)
	}
}
