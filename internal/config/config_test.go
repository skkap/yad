package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolve(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, env := range []string{"YAD_CONFIG_DIR", "YAD_DATA_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
		t.Setenv(env, "")
	}
	for _, tc := range []struct {
		profile, config, data string
	}{
		{"", filepath.Join(home, ".config/yad"), filepath.Join(home, ".local/share/yad")},
		{"default", filepath.Join(home, ".config/yad"), filepath.Join(home, ".local/share/yad")},
		{"work", filepath.Join(home, ".config/yad/profiles/work"), filepath.Join(home, ".local/share/yad/profiles/work")},
	} {
		p, err := Resolve(tc.profile)
		if err != nil {
			t.Fatal(err)
		}
		if p.Config != tc.config || p.Data != tc.data {
			t.Errorf("Resolve(%q) = %+v", tc.profile, p)
		}
	}
	if _, err := Resolve("../etc"); err == nil {
		t.Error("a path-shaped profile name was accepted")
	}

	t.Setenv("YAD_CONFIG_DIR", "/c")
	t.Setenv("YAD_DATA_DIR", "/d")
	if p, _ := Resolve("work"); p.Config != "/c" || p.Data != "/d" {
		t.Errorf("overrides ignored: %+v", p)
	}
}

func testPaths(t *testing.T) Paths {
	t.Helper()
	t.Setenv("YAD_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	t.Setenv("YAD_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	p, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingIsDefault(t *testing.T) {
	c, err := Load(testPaths(t))
	if err != nil {
		t.Fatal(err)
	}
	if c.Capacity != DefaultCapacity || c.Sessions.IdleTTL.Duration != DefaultIdleTTL {
		t.Errorf("defaults = %+v", c)
	}
}

// The example in ARCHITECTURE.md §4 is what owners will copy, so it must load.
func TestLoadArchitectureExample(t *testing.T) {
	p := testPaths(t)
	src := `
name     = "ashikaga"
labels   = ["macos", "home"]
capacity = 4

[harness.claude]
permission_mode = "bypassPermissions"
cap             = 3
accounts        = ["personal", "family"]

[harness.codex]
sandbox  = "danger-full-access"
approval = "never"
cap      = 2
accounts = ["personal"]

[[connection]]
name = "yashiki"
url  = "https://ashikaga.tail.ts.net/yad/v1"
cap  = 2

[sessions]
idle_ttl = "336h"
`
	if err := os.MkdirAll(p.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Harness["claude"].Accounts[1] != "family" || c.Connections[0].Cap != 2 || c.Sessions.IdleTTL.Duration != 336*time.Hour {
		t.Errorf("loaded %+v", c)
	}
	if c.Supervise.Inactivity.Duration != DefaultInactivity {
		t.Errorf("an unset section lost its default: %v", c.Supervise.Inactivity)
	}

	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	again, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if again.Name != "ashikaga" || again.Harness["codex"].Sandbox != "danger-full-access" {
		t.Errorf("save/load lost data: %+v", again)
	}
	assertPrivate(t, p.ConfigFile())
}

func TestLoadRefuses(t *testing.T) {
	for name, src := range map[string]string{
		"unknown key":       "capacity = 2\npermision_mode = \"x\"\n",
		"zero capacity":     "capacity = 0\n",
		"duplicate account": "[harness.claude]\naccounts = [\"a\", \"a\"]\n",
		"bad duration":      "[sessions]\nidle_ttl = \"two weeks\"\n",
		"connection no url": "[[connection]]\nname = \"x\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := testPaths(t)
			os.MkdirAll(p.Config, 0o700)
			os.WriteFile(p.ConfigFile(), []byte(src), 0o600)
			if _, err := Load(p); err == nil {
				t.Errorf("accepted:\n%s", src)
			}
		})
	}
}

func TestRunnerIDIsStable(t *testing.T) {
	p := testPaths(t)
	a, err := p.RunnerID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := p.RunnerID()
	if a == "" || a != b {
		t.Errorf("ids %q then %q", a, b)
	}
	assertPrivate(t, filepath.Join(p.Config, "runner-id"))
}

func TestCredentials(t *testing.T) {
	p := testPaths(t)
	if _, err := p.Credential("zumino"); err == nil || !strings.Contains(err.Error(), "yad connect") {
		t.Errorf("missing credential error = %v", err)
	}
	if err := p.SaveCredential("zumino", "yad_cred_secret"); err != nil {
		t.Fatal(err)
	}
	got, err := p.Credential("zumino")
	if err != nil || got != "yad_cred_secret" {
		t.Errorf("Credential = %q, %v", got, err)
	}
	path := p.credentialPath("zumino")
	assertPrivate(t, path)

	os.Chmod(path, 0o644)
	if _, err := p.Credential("zumino"); err == nil {
		t.Error("a world-readable credential was used")
	} else if strings.Contains(err.Error(), "yad_cred_secret") {
		t.Error("the error printed the token")
	}
	if err := p.SaveCredential("../x", "t"); err == nil {
		t.Error("a path-shaped connection name was accepted")
	}
	if err := p.DeleteCredential("zumino"); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteCredential("zumino"); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

func assertPrivate(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("%s is %v, want 0600", path, fi.Mode().Perm())
	}
}
