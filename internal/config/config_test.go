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
	if c.Capacity != DefaultCapacity || c.Sessions.IdleTTL.Duration != DefaultIdleTTL || c.Sessions.DiskFloor != DefaultDiskFloor {
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
idle_ttl   = "336h"
disk_floor = "10GiB"
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
	if c.Sessions.DiskFloor != 10<<30 {
		t.Errorf("disk_floor = %d, want 10 GiB", c.Sessions.DiskFloor)
	}
	if c.Supervise.Inactivity.Duration != DefaultInactivity {
		t.Errorf("an unset section lost its default: %v", c.Supervise.Inactivity)
	}
	if c.Drain.Wait.Duration != DefaultDrainWait {
		t.Errorf("an unset drain wait is %v, want the default %v", c.Drain.Wait, DefaultDrainWait)
	}

	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	again, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if again.Name != "ashikaga" || again.Harness["codex"].Sandbox != "danger-full-access" || again.Sessions.DiskFloor != 10<<30 {
		t.Errorf("save/load lost data: %+v", again)
	}
	assertPrivate(t, p.ConfigFile())
}

func TestLoadRefuses(t *testing.T) {
	for name, src := range map[string]string{
		"unknown key":       "capacity = 2\npermision_mode = \"x\"\n",
		"zero capacity":     "capacity = 0\n",
		"negative drain":    "[drain]\nwait = \"-1m\"\n",
		"duplicate account": "[harness.claude]\naccounts = [\"a\", \"a\"]\n",
		"bad duration":      "[sessions]\nidle_ttl = \"two weeks\"\n",
		"negative idle ttl": "[sessions]\nidle_ttl = \"-1h\"\n",
		"bad size":          "[sessions]\ndisk_floor = \"lots\"\n",
		"negative size":     "[sessions]\ndisk_floor = \"-5GB\"\n",
		"size overflow":     "[sessions]\ndisk_floor = \"99999999999TiB\"\n",
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

// An id file that exists but cannot be read is not a missing one: replacing it
// would orphan every session the runner held.
func TestRunnerIDRefusesUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	p := testPaths(t)
	first, err := p.RunnerID()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.Config, "runner-id")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) })
	if id, err := p.RunnerID(); err == nil {
		t.Fatalf("got a new id %q instead of an error", id)
	}
	os.Chmod(path, 0o600)
	if again, _ := p.RunnerID(); again != first {
		t.Errorf("identity changed from %q to %q", first, again)
	}
}

func TestCheckCredential(t *testing.T) {
	p := testPaths(t)
	write := func(body string, mode os.FileMode) {
		t.Helper()
		dir := filepath.Join(p.Config, "credentials")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "home"), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, "home"), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.CheckCredential("home"); err == nil || !strings.Contains(err.Error(), "yad connect") {
		t.Errorf("missing: %v", err)
	}
	for _, tc := range []struct {
		name, body string
		mode       os.FileMode
		want       string // "" is accepted
	}{
		{"good", "yadrun_abc123\n", 0o600, ""},
		{"exposed", "yadrun_abc123\n", 0o644, "readable by others"},
		{"empty", "\n", 0o600, "is empty"},
		{"two words", "yadrun_abc 123\n", 0o600, "not a single token"},
		{"two lines", "yadrun_abc\nsecond\n", 0o600, "not a single token"},
		{"non-ascii", "yadrun_abé\n", 0o600, "not a single token"},
	} {
		write(tc.body, tc.mode)
		err := p.CheckCredential("home")
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		case err != nil && strings.Contains(err.Error(), "yadrun_ab"):
			t.Errorf("%s: the error carries the credential: %v", tc.name, err)
		}
	}
}

func TestByteSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want ByteSize
		out  string
	}{
		{"0", 0, "0"},
		{"5GiB", 5 << 30, "5GiB"},
		{"500MB", 500e6, "500MB"},
		{"1536 KiB", 1536 << 10, "1536KiB"},
		{"2TB", 2e12, "2TB"},
		{"123", 123, "123B"},
	} {
		var b ByteSize
		if err := b.UnmarshalText([]byte(tc.in)); err != nil || b != tc.want {
			t.Errorf("%q = %d, %v; want %d", tc.in, b, err, tc.want)
			continue
		}
		if out, _ := b.MarshalText(); string(out) != tc.out {
			t.Errorf("%d marshals as %q, want %q", b, out, tc.out)
		}
	}
}
