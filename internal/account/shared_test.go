package account

import (
	"os"
	"path/filepath"
	"testing"
)

// An account's home takes the owner's instructions, settings and skills from
// the harness's default home, so a run behaves the same whichever account is
// free (decision 0054). What the account has of its own is left alone, and a
// link whose source the owner removed goes with it.
func TestAnAccountHomeSharesTheMachinesConfig(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	def := filepath.Join(user, ".claude")
	for _, d := range []string{def, filepath.Join(def, "skills", "one")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"CLAUDE.md": "@~/AGENTS.md\n", "settings.json": "{}", ".credentials.json": "{}"} {
		if err := os.WriteFile(filepath.Join(def, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data := t.TempDir()
	// The account already has settings of its own before the first run.
	home := HomeDir(data, "claude", "tl")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"own":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(data, "claude", "tl"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLAUDE.md", "skills"} {
		at, err := os.Readlink(filepath.Join(home, name))
		if err != nil || at != filepath.Join(def, name) {
			t.Errorf("%s: link %q, %v — want it to point at the default home's", name, at, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(home, "settings.json")); string(b) != `{"own":true}` {
		t.Errorf("the account's own settings.json was replaced: %s", b)
	}
	// Never a credential.
	if _, err := os.Lstat(filepath.Join(home, ".credentials.json")); err == nil {
		t.Error("the default home's credential was linked into the account")
	}
	// Idempotent, as every run's preparation calls it.
	if _, err := Ensure(data, "claude", "tl"); err != nil {
		t.Fatal(err)
	}
	// The owner removes the machine's CLAUDE.md: the next run's home no
	// longer points at nothing.
	if err := os.Remove(filepath.Join(def, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(data, "claude", "tl"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, "CLAUDE.md")); err == nil {
		t.Error("a link to a removed CLAUDE.md was kept")
	}
}

// The machine's plugins reach every account, as its settings — which enable
// them by name — already do (YAD-174). Claude makes an empty plugins/ of its
// own in a home that has none, so an account that ran before plugins were
// shared has one; that empty default becomes the link at its next run, and a
// plugins/ holding anything else is the owner's for that account and stays.
func TestAnAccountHomeSharesTheMachinesPlugins(t *testing.T) {
	installed := `{"version":2,"plugins":{"revmux@revmux":[{"scope":"user","installPath":"/x"}]}}`
	for _, tc := range []struct {
		name string
		// shape is what the account's home holds at plugins/ before its next
		// run: "" for nothing, "dir" for a directory holding own (a file
		// name to its body, "/" for an empty directory), "link" for a link
		// elsewhere, "file" for a plain file.
		shape  string
		own    map[string]string
		linked bool
	}{
		{name: "none yet", linked: true},
		{name: "claude's empty default", shape: "dir", own: map[string]string{"installed_plugins.json": "{}"}, linked: true},
		{name: "claude's empty default, versioned, with empty marketplaces and cache", shape: "dir", own: map[string]string{
			"installed_plugins.json": `{"version":2,"plugins":{}}`, "known_marketplaces.json": "{}\n", "cache": "/", "marketplaces": "/",
		}, linked: true},
		{name: "an empty directory", shape: "dir", own: map[string]string{}, linked: true},
		{name: "a plugin installed for this account", shape: "dir", own: map[string]string{"installed_plugins.json": installed}},
		{name: "a marketplace of the account's own", shape: "dir", own: map[string]string{
			"installed_plugins.json": "{}", "known_marketplaces.json": `{"mine":{"source":{"source":"github","repo":"a/b"}}}`,
		}},
		{name: "something else in it", shape: "dir", own: map[string]string{"installed_plugins.json": "{}", "data/x/state": "1"}},
		{name: "an installed list that is not JSON", shape: "dir", own: map[string]string{"installed_plugins.json": "{"}},
		{name: "a link of the owner's", shape: "link"},
		{name: "a file", shape: "file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := t.TempDir()
			t.Setenv("HOME", user)
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			def := filepath.Join(user, ".claude")
			writeTree(t, filepath.Join(def, "plugins"), map[string]string{"installed_plugins.json": installed})
			data := t.TempDir()
			home := HomeDir(data, "claude", "tl")
			own := filepath.Join(home, "plugins")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			elsewhere := t.TempDir()
			switch tc.shape {
			case "dir":
				if err := os.MkdirAll(own, 0o700); err != nil {
					t.Fatal(err)
				}
				writeTree(t, own, tc.own)
			case "link":
				if err := os.Symlink(elsewhere, own); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(own, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// Twice: every run prepares the home.
			for range 2 {
				if _, err := Ensure(data, "claude", "tl"); err != nil {
					t.Fatal(err)
				}
			}
			at, linkErr := os.Readlink(own)
			if tc.linked {
				if linkErr != nil || at != filepath.Join(def, "plugins") {
					t.Fatalf("plugins/ is %q (%v), want a link to the default home's", at, linkErr)
				}
				return
			}
			switch tc.shape {
			case "link":
				if at != elsewhere {
					t.Errorf("the owner's link now points at %q", at)
				}
			case "file":
				if b, _ := os.ReadFile(own); string(b) != "x" {
					t.Errorf("the file was replaced: %q", b)
				}
			default:
				if linkErr == nil {
					t.Fatalf("plugins/ of the account's own was replaced by a link to %s", at)
				}
				for name, body := range tc.own {
					if body == "/" {
						continue
					}
					if b, err := os.ReadFile(filepath.Join(own, name)); err != nil || string(b) != body {
						t.Errorf("%s is %q (%v), want it kept as %q", name, b, err, body)
					}
				}
			}
		})
	}
}

// Two runs replacing one home's empty plugins/ at once: the other run wins in
// the gap between this one's look inside and its removals, and leaves the
// link to the machine's plugins at the path. What this run then removes must
// be in the directory it looked at, never through that link.
func TestReplacingAnEmptyPluginsDirNeverDeletesThroughTheWinnersLink(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	def := filepath.Join(user, ".claude", "plugins")
	installed := `{"version":2,"plugins":{"revmux@revmux":[{"scope":"user"}]}}`
	writeTree(t, def, map[string]string{"installed_plugins.json": installed, "known_marketplaces.json": "{}"})
	data := t.TempDir()
	home := HomeDir(data, "claude", "tl")
	own := filepath.Join(home, "plugins")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTree(t, own, map[string]string{"installed_plugins.json": "{}", "known_marketplaces.json": "{}"})
	won := false
	beforeEmptyRemove = func(dst string) {
		// The winner, by path, as a run doing exactly this would.
		for _, f := range []string{"installed_plugins.json", "known_marketplaces.json"} {
			if err := os.Remove(filepath.Join(dst, f)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Remove(dst); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(def, dst); err != nil {
			t.Fatal(err)
		}
		won = true
	}
	t.Cleanup(func() { beforeEmptyRemove = nil })
	if _, err := Ensure(data, "claude", "tl"); err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Fatal("the race was never staged; this test would pass vacuously")
	}
	if b, err := os.ReadFile(filepath.Join(def, "installed_plugins.json")); err != nil || string(b) != installed {
		t.Errorf("the machine's installed_plugins.json is %q (%v): deleted through the link", b, err)
	}
	if _, err := os.Stat(filepath.Join(def, "known_marketplaces.json")); err != nil {
		t.Errorf("the machine's known_marketplaces.json is gone: %v", err)
	}
	if at, err := os.Readlink(own); err != nil || at != def {
		t.Errorf("plugins/ is %q (%v), want the link", at, err)
	}
}

// A machine with no plugins of its own links none, and the empty plugins/
// Claude makes in the account's home is left for it.
func TestNoMachinePluginsLeavesTheHomesOwn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	data := t.TempDir()
	home := HomeDir(data, "claude", "tl")
	if err := os.MkdirAll(filepath.Join(home, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(data, "claude", "tl"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(home, "plugins")); err != nil || !fi.IsDir() {
		t.Errorf("plugins/ is %v (%v), want the home's own directory kept", fi, err)
	}
}

// writeTree writes files under dir, a body of "/" making an empty directory.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if body == "/" {
			if err := os.MkdirAll(p, 0o700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
