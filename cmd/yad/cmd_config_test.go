package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// `yad config apply` is what yad-machine up runs on every up: it writes the
// spec where there is no config.toml, says each setting it changed, keeps the
// connection yad connect wrote, and the third time — nothing new in the spec
// — says so and writes nothing.
func TestConfigApplySaysWhatItChanged(t *testing.T) {
	p := accountEnv(t)
	spec := filepath.Join(t.TempDir(), "spec.toml")
	write := func(src string) {
		t.Helper()
		if err := os.WriteFile(spec, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("name = \"m\"\nlabels = [\"linux\"]\n[harness.claude]\naccounts = [\"main\"]\n")

	code, out, errs := yadIn(t, "config", "apply", spec)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "wrote "+p.ConfigFile()+" from "+spec) {
		t.Errorf("first apply said %q", out)
	}
	shellwordtest.CheckEnv(t, onlyCommand(t, out, "yad --profile default service install"), dirsEnv(t),
		"yad", "--profile", "default", "service", "install")

	// What yad connect writes, between two ups.
	if _, err := config.Update(t.Context(), p, func(c *config.Config) (bool, error) {
		c.Connections = append(c.Connections, config.Connection{Name: "zumino", URL: "https://zumino.cc/api/yad/v1"})
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	write("name = \"m\"\nlabels = [\"linux\", \"tl\"]\n[harness.claude]\naccounts = [\"main\", \"tl\"]\n")
	code, out, errs = yadIn(t, "config", "apply", spec)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{
		`harness.claude.accounts = ["main", "tl"] (was ["main"])`,
		`labels = ["linux", "tl"] (was ["linux"])`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("second apply said\n%s\nwant a line %q", out, want)
		}
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Connections) != 1 {
		t.Errorf("connections = %+v, want zumino kept", c.Connections)
	}

	code, out, errs = yadIn(t, "config", "apply", spec)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "already matches") || strings.Contains(out, "service install") {
		t.Errorf("an apply with nothing new said %q", out)
	}
}

// A spec with a [[connection]] is not an error — the machine's own are kept
// — but its owner is told why it was not taken, and how one is made.
func TestConfigApplySaysASpecsConnectionIsNotTaken(t *testing.T) {
	accountEnv(t)
	spec := filepath.Join(t.TempDir(), "spec.toml")
	if err := os.WriteFile(spec, []byte("[[connection]]\nname = \"h\"\nurl = \"https://hub.example.com/v1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := yadIn(t, "config", "apply", spec)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	shellwordtest.CheckEnv(t, onlyCommand(t, out, "yad --profile default connect"), dirsEnv(t),
		"yad", "--profile", "default", "connect", "<hub url>", "--token", "-")
}

func TestConfigApplyRefusesNonsense(t *testing.T) {
	accountEnv(t)
	for _, args := range [][]string{
		{"config"},
		{"config", "merge", "x"},
		{"config", "apply"},
		{"config", "apply", filepath.Join(t.TempDir(), "missing.toml")},
	} {
		if code, _, _ := yadIn(t, args...); code == 0 {
			t.Errorf("yad %q exited 0", args)
		}
	}
}
