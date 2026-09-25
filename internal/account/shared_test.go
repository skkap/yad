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
