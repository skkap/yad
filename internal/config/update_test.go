package config

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func writeConfig(t *testing.T, p Paths, src string) {
	t.Helper()
	if err := os.MkdirAll(p.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

// manage_accounts is the owner's per connection, and absent is yes (0057).
func TestManageAccountsIsYesUnlessTheOwnerSaysNo(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, `
[[connection]]
name = "zumino"
url  = "https://zumino.cc/api/yad/v1"

[[connection]]
name = "work"
url  = "https://hub.example.com/v1"
manage_accounts = false

[[connection]]
name = "home"
url  = "https://home.example.com/v1"
manage_accounts = true
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"zumino": true, "work": false, "home": true}
	for _, conn := range c.Connections {
		if got := conn.MayManageAccounts(); got != want[conn.Name] {
			t.Errorf("connection %s may manage accounts: %v, want %v", conn.Name, got, want[conn.Name])
		}
	}
	// Written back as the owner wrote it: an absent one stays absent.
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p.ConfigFile())
	if n := strings.Count(string(b), "manage_accounts"); n != 2 {
		t.Errorf("config.toml names manage_accounts %d times after a save, want the owner's 2:\n%s", n, b)
	}
}

// The daemon's copy of config.toml is from when it started. An account a hub
// adds is written into the file as it reads now, so an edit the owner made by
// hand since is kept, and only the one list changes.
func TestUpdateAccountsKeepsWhatElseTheFileSays(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, "capacity = 4\n[harness.claude]\naccounts = [\"work\"]\n")
	started, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	// The owner, by hand, while the daemon runs.
	writeConfig(t, p, "capacity = 7\nlabels = [\"gpu\"]\n[harness.claude]\naccounts = [\"work\"]\ncap = 2\n")

	got, err := UpdateAccounts(context.Background(), p, "claude", WithAccount("second"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []Config{got, again} {
		if c.Capacity != 7 || !slices.Equal(c.Labels, []string{"gpu"}) || c.Harness["claude"].Cap != 2 {
			t.Errorf("the hand edit is lost: %+v", c)
		}
		if !slices.Equal(c.Harness["claude"].Accounts, []string{"work", "second"}) {
			t.Errorf("accounts are %v, want the new one last", c.Harness["claude"].Accounts)
		}
	}
	if started.Capacity != 4 {
		t.Fatal("the test did not start from the file before the hand edit")
	}

	// Taking off a label the file does not list writes nothing.
	before, _ := os.Stat(p.ConfigFile())
	time.Sleep(20 * time.Millisecond)
	if _, err := UpdateAccounts(context.Background(), p, "claude", WithoutAccount("stranger")); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(p.ConfigFile()); !after.ModTime().Equal(before.ModTime()) {
		t.Error("a change that changed nothing rewrote config.toml")
	}
}

// Writers at once never lose each other's change: every label each one
// added is there at the end, and every one each removed is gone.
func TestConcurrentAccountWritesLoseNothing(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, "[harness.claude]\naccounts = [\"gone-a\", \"gone-b\", \"stays\"]\n")
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 24 {
		wg.Go(func() {
			h := []string{"claude", "codex"}[i%2]
			if _, err := UpdateAccounts(ctx, p, h, WithAccount(fmt.Sprintf("acct-%d", i))); err != nil {
				errs <- err
			}
		})
	}
	for _, gone := range []string{"gone-a", "gone-b"} {
		wg.Go(func() {
			if _, err := UpdateAccounts(ctx, p, "claude", WithoutAccount(gone)); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	all := append(slices.Clone(c.Harness["claude"].Accounts), c.Harness["codex"].Accounts...)
	for i := range 24 {
		if !slices.Contains(all, fmt.Sprintf("acct-%d", i)) {
			t.Errorf("acct-%d was lost: %v", i, all)
		}
	}
	if slices.Contains(all, "gone-a") || slices.Contains(all, "gone-b") || !slices.Contains(all, "stays") {
		t.Errorf("the removals did not all land, or took too much: %v", all)
	}
}

// A lock held by something wedged is an error naming what to do, not a hang.
func TestUpdateGivesUpOnALockHeldTooLong(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, "")
	f, err := os.OpenFile(p.lockFile(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = UpdateAccounts(ctx, p, "claude", WithAccount("work"))
	if err == nil || !strings.Contains(err.Error(), "run this again") {
		t.Fatalf("err = %v, want one saying to run it again", err)
	}
}
