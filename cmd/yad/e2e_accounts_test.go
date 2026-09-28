package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hub"
	"github.com/skkap/yad/internal/hubapiclient"
)

// Accounts added and removed from a hub (decision 0057), end to end through
// the operator's commands against a running daemon.

func (m *machine) configured(t *testing.T) config.Config {
	t.Helper()
	c, err := config.Load(config.Paths{Config: m.p.config, Data: m.p.data})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// `yad hub login token --add` adds an account the runner did not list, and
// `yad hub account remove` removes it: each waits until the runner has done
// it, and each shows in config.toml and in the daemon's own account list.
func TestHubAddsAndRemovesAnAccountThroughTheCLI(t *testing.T) {
	m := newMachine(t, claudeE2E)
	old := loginPoll
	loginPoll = 20 * time.Millisecond
	t.Cleanup(func() { loginPoll = old })
	m.listAccounts("claude", "work")
	d := m.daemon()
	id := m.runnerID()

	const tok = "sk-ant-oat01-added-from-the-hub"
	code, out, errs := m.p.yad(tok+"\n", "hub", "login", "token", "--add", "--hub", m.service, id, "claude", "second")
	if code != 0 || !strings.Contains(out, "is added to runner") {
		t.Fatalf("add: exit %d\n%s\n%s\ndaemon:\n%s", code, out, errs, d.out.String())
	}
	if got := m.configured(t).Harness["claude"].Accounts; !slices.Equal(got, []string{"work", "second"}) {
		t.Errorf("config.toml lists %v", got)
	}
	if list := m.ok("account", "list", "--json"); !strings.Contains(strings.Join(strings.Fields(list), ""), `"label":"second","state":"free"`) {
		t.Errorf("the added account is not free:\n%s", list)
	}
	if strings.Contains(out+errs+d.out.String(), tok) {
		t.Error("the token was printed")
	}

	// An add of a label already listed is the runner's refusal, with what to
	// do instead.
	code, _, errs = m.p.yad(tok+"\n", "hub", "login", "token", "--add", "--hub", m.service, id, "claude", "work")
	if code == 0 || !strings.Contains(errs, "already listed") {
		t.Errorf("an add of a listed label: exit %d: %s", code, errs)
	}

	out = m.ok("hub", "account", "remove", "--hub", m.service, id, "claude", "second")
	if !strings.Contains(out, "is removed from runner") {
		t.Fatalf("remove printed %q", out)
	}
	if got := m.configured(t).Harness["claude"].Accounts; !slices.Equal(got, []string{"work"}) {
		t.Errorf("config.toml lists %v after the removal", got)
	}
	if _, err := os.Stat(account.HomeDir(m.p.data, "claude", "second")); !os.IsNotExist(err) {
		t.Errorf("the removed account's home is still there (%v)", err)
	}
	// Removed already: the same command answers at once.
	m.ok("hub", "account", "remove", "--hub", m.service, id, "claude", "second")
}

// The CLI and the daemon both write config.toml's account lists, at once:
// accounts added from a hub while the owner removes others at the terminal,
// and an edit the owner made by hand after the daemon started. Nothing is
// lost — every add is listed, every removal gone, the hand edit kept.
func TestTheCLIAndTheDaemonWriteAccountsAtOnce(t *testing.T) {
	m := newMachine(t, claudeE2E)
	var gone []string
	for i := range 4 {
		gone = append(gone, fmt.Sprintf("old-%d", i))
	}
	m.listAccounts("claude", append([]string{"work"}, gone...)...)
	d := m.daemon()
	id := m.runnerID()
	ctx := context.Background()
	admin, err := hub.IssueAdminToken(ctx, m.hubDB, "e2e", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	api, err := hubapiclient.New(m.service, admin)
	if err != nil {
		t.Fatal(err)
	}
	// Once the daemon is syncing: an add to a runner the hub has not heard
	// advertise accounts is refused.
	eventually(t, "the daemon has synced", func() bool {
		r, err := api.Runner(ctx, id)
		return err == nil && r.LastSyncAt != nil
	})

	// By hand, while the daemon runs: not in the copy it started with.
	p := config.Paths{Config: m.p.config, Data: m.p.data}
	hand := m.configured(t)
	hand.Labels = []string{"edited-by-hand"}
	if err := config.Save(p, hand); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var added []string
	for i := range 4 {
		label := fmt.Sprintf("hub-%d", i)
		added = append(added, label)
		wg.Go(func() {
			l, err := api.StartLogin(ctx, id, hubapi.LoginRequest{Harness: "claude", Account: label, Add: true,
				Token: "sk-ant-oat01-" + label})
			if err != nil {
				t.Error(err)
				return
			}
			within(t, 30*time.Second, func() bool {
				l, err = api.Login(ctx, id, l.LoginID)
				return err == nil && l.State.Terminal()
			})
			if l.State != hubapi.LoginState(v1.LoginSucceeded) {
				t.Errorf("the add of %s ended %s: %s", label, l.State, l.Error)
			}
		})
	}
	for _, label := range gone {
		wg.Go(func() {
			var out, errb bytes.Buffer
			if code := run(ctx, []string{"account", "remove", "claude", label, "--yes"}, &out, &errb); code != 0 {
				t.Errorf("yad account remove %s: exit %d: %s", label, code, errb.String())
			}
		})
	}
	wg.Wait()
	if t.Failed() {
		t.Fatalf("daemon:\n%s", d.out.String())
	}

	c := m.configured(t)
	got := c.Harness["claude"].Accounts
	for _, want := range append([]string{"work"}, added...) {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not listed: %v", want, got)
		}
	}
	for _, label := range gone {
		if slices.Contains(got, label) {
			t.Errorf("%s is still listed: %v", label, got)
		}
	}
	if !slices.Equal(c.Labels, []string{"edited-by-hand"}) {
		t.Errorf("the hand edit is lost: labels %v", c.Labels)
	}
}

// within is eventually for a goroutine, where a test may not stop: it
// reports and returns.
func within(t *testing.T, d time.Duration, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !done() {
		if time.Now().After(deadline) {
			t.Error("gave up waiting")
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
