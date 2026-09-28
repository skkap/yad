package config

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func specFile(t *testing.T, src string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// What yad wrote on the machine — a connection, an account logged in there
// or by a hub — survives a spec that knows nothing of it; everything else
// comes from the spec.
func TestApplyTakesTheSpecAndKeepsWhatTheMachineWrote(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, `
name     = "old"
labels   = ["linux"]
capacity = 2

[harness.claude]
cap      = 2
accounts = ["main", "second"]

[harness.codex]
cap      = 1
sandbox  = "read-only"
accounts = ["work"]

[harness.gemini]
cap = 3

[[connection]]
name = "zumino"
url  = "https://zumino.cc/api/yad/v1"

[drain]
wait = "1h"
`)
	spec := specFile(t, `
name     = "tl-general"
labels   = ["linux", "tl"]
capacity = 3

[harness.claude]
cap      = 3
accounts = ["tl", "main"]

[[connection]]
name = "elsewhere"
url  = "https://hub.example.com/v1"
`)
	got, err := Apply(context.Background(), p, spec)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "tl-general" || !slices.Equal(c.Labels, []string{"linux", "tl"}) || c.Capacity != 3 {
		t.Errorf("name, labels, capacity = %q %v %d, want the spec's", c.Name, c.Labels, c.Capacity)
	}
	// The spec's accounts in its order, then the one only the machine has.
	if h := c.Harness["claude"]; h.Cap != 3 || !slices.Equal(h.Accounts, []string{"tl", "main", "second"}) {
		t.Errorf("harness.claude = %+v, want cap 3 and accounts [tl main second]", h)
	}
	// No section in the spec: the settings go, the account logged in stays.
	if h := c.Harness["codex"]; h.Cap != 0 || h.Sandbox != "" || !slices.Equal(h.Accounts, []string{"work"}) {
		t.Errorf("harness.codex = %+v, want only accounts [work]", h)
	}
	if _, ok := c.Harness["gemini"]; ok {
		t.Errorf("harness.gemini kept, with no account and no section in the spec")
	}
	if len(c.Connections) != 1 || c.Connections[0].Name != "zumino" {
		t.Errorf("connections = %+v, want the machine's zumino and not the spec's", c.Connections)
	}
	if c.Drain.Wait.Duration != DefaultDrainWait {
		t.Errorf("drain.wait = %s, want the default the spec leaves it at", c.Drain.Wait.Duration)
	}

	if got.Created {
		t.Error("Created, for a file that was there")
	}
	var keys []string
	for _, ch := range got.Changes {
		keys = append(keys, ch.Key)
	}
	want := []string{
		"capacity", "drain.wait",
		"harness.claude.accounts", "harness.claude.cap",
		"harness.codex.cap", "harness.codex.sandbox", "harness.gemini.cap",
		"labels", "name",
	}
	if !slices.Equal(keys, want) {
		t.Errorf("changes name %v, want %v", keys, want)
	}
	for _, ch := range got.Changes {
		if ch.Key == "harness.claude.accounts" && ch.String() != `harness.claude.accounts = ["tl", "main", "second"] (was ["main", "second"])` {
			t.Errorf("the accounts change reads %q", ch.String())
		}
		if ch.Key == "harness.codex.sandbox" && ch.String() != `harness.codex.sandbox removed (was "read-only")` {
			t.Errorf("a removed setting reads %q", ch.String())
		}
	}
}

// A second apply of the same spec writes nothing: the kit restarts the runner
// only when the file changed, and a restart drains every run it holds. The
// spec's empty lists are the trap — the file cannot say "empty" apart from
// "unset", so reading it back must not look like a change.
func TestApplyTwiceChangesNothingTheSecondTime(t *testing.T) {
	p := testPaths(t)
	spec := specFile(t, `
name     = "tl-general"
labels   = []
capacity = 2

[harness.claude]
accounts = []

[harness.codex]

[sessions]
idle_ttl = "0s"

[workdirs]
roots = []
`)
	writeConfig(t, p, "capacity = 4\n[[connection]]\nname = \"zumino\"\nurl = \"https://zumino.cc/api/yad/v1\"\n")
	first, err := Apply(context.Background(), p, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed() {
		t.Fatal("the first apply changed nothing")
	}
	before, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1_000_000, 0)
	if err := os.Chtimes(p.ConfigFile(), old, old); err != nil {
		t.Fatal(err)
	}
	second, err := Apply(context.Background(), p, spec)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed() {
		t.Errorf("the second apply changed %v", second.Changes)
	}
	after, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || !st.ModTime().Equal(old) {
		t.Errorf("config.toml was written again:\n%s", after)
	}
}

// An account the spec stops listing is not removed: it may be one added at the
// machine, and a removal has to reach the daemon, which only
// `yad account remove` does (0043).
func TestApplyNeverRemovesAnAccount(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, "[harness.claude]\naccounts = [\"main\", \"old\"]\n")
	got, err := Apply(context.Background(), p, specFile(t, "[harness.claude]\naccounts = [\"main\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Harness["claude"].Accounts, []string{"main", "old"}) {
		t.Errorf("accounts = %v, want both kept", c.Harness["claude"].Accounts)
	}
	if got.Changed() {
		t.Errorf("changes %v, for a spec that adds nothing", got.Changes)
	}
}

// A machine's first up has no config.toml: Apply writes the spec's, private.
func TestApplyWritesTheSpecWhereThereIsNoFile(t *testing.T) {
	p := testPaths(t)
	spec := specFile(t, "name = \"m\"\nlabels = [\"a\"]\n[harness.claude]\naccounts = [\"main\"]\n")
	got, err := Apply(context.Background(), p, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Created || len(got.Changes) != 0 {
		t.Errorf("applied %+v, want Created and no change list", got)
	}
	st, err := os.Stat(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("config.toml is %o, want 0600", st.Mode().Perm())
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "m" || !slices.Equal(c.Harness["claude"].Accounts, []string{"main"}) {
		t.Errorf("config.toml holds %+v, want the spec's", c)
	}
}

// The spec is read strictly, as config.toml is: a typo in a permission
// setting must fail the apply, naming the file, not vanish.
func TestReadFileRefusesAnUnknownSettingAndAMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[harness.claude]\npermision_mode = \"plan\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(path); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "permision_mode") {
		t.Errorf("ReadFile of a typo: %v, want an unknown-setting error naming the file", err)
	}
	if _, err := ReadFile(filepath.Join(dir, "missing.toml")); err == nil {
		t.Error("ReadFile of a missing file succeeded")
	}
}
