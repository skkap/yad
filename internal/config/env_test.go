package config

import (
	"context"
	"strings"
	"testing"
)

// [env] is a table of strings that loads, saves and loads again as written —
// no $VAR or ~ expanded.
func TestEnvRoundTrips(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, `
[env]
DOCKER_HOST         = "unix://~/.colima/default/docker.sock"
BASH_MAX_TIMEOUT_MS = "1800000"
GOFLAGS             = "$HOME"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"DOCKER_HOST": "unix://~/.colima/default/docker.sock", "BASH_MAX_TIMEOUT_MS": "1800000", "GOFLAGS": "$HOME"}
	if len(c.Env) != len(want) {
		t.Fatalf("env has %d entries, want %d", len(c.Env), len(want))
	}
	for k, v := range want {
		if c.Env[k] != v {
			t.Errorf("env.%s did not load as written", k)
		}
	}
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	again, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range want {
		if again.Env[k] != v {
			t.Errorf("env.%s did not survive a save", k)
		}
	}
}

// What [env] may not set is refused at load, naming the variable and the
// rule, and never quoting the value.
func TestEnvRefused(t *testing.T) {
	const value = "do-not-print-this-value"
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"an account variable", `ANTHROPIC_API_KEY = "` + value + `"`, []string{"env.ANTHROPIC_API_KEY", "0060", "add an account"}},
		{"IS_SANDBOX", `IS_SANDBOX = "` + value + `"`, []string{"env.IS_SANDBOX", "root check", "where the runner starts"}},
		{"a CLAUDE_CODE_ setting", `CLAUDE_CODE_FOO = "` + value + `"`, []string{"env.CLAUDE_CODE_FOO", "settings.json"}},
		{"PATH", `PATH = "` + value + `"`, []string{"env.PATH", "install the service again"}},
		{"a hook variable", `WT_SLOT = "` + value + `"`, []string{"env.WT_SLOT", "setup hook"}},
		{"a name that is not a variable", `"NOT A NAME" = "` + value + `"`, []string{"is not a variable name"}},
		{"an unquoted number", `BASH_MAX_TIMEOUT_MS = 1800000`, []string{"env.BASH_MAX_TIMEOUT_MS is not a string", "quote it"}},
		{"a boolean", `SOME_FLAG = true`, []string{"env.SOME_FLAG is not a string"}},
		{"a table", "[env.NESTED]\nX = \"" + value + `"`, []string{"env.NESTED is not a string"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			src := "[env]\n" + tc.src + "\n"
			if strings.HasPrefix(tc.src, "[") {
				src = tc.src + "\n"
			}
			writeConfig(t, p, src)
			_, err := Load(p)
			if err == nil {
				t.Fatal("loaded")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the error does not say %q", w)
				}
			}
			if strings.Contains(err.Error(), value) {
				t.Error("the error quotes the value")
			}
		})
	}
}

// A NUL byte in a value is refused at load: no child could start with it.
func TestEnvRefusesNUL(t *testing.T) {
	c := Default()
	c.Env = map[string]string{"X": "a\x00b"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Errorf("Validate = %v", err)
	}
}

// `yad config apply` takes a spec's [env] like any other setting and reports
// each change by name alone.
func TestApplyReportsEnvByNameOnly(t *testing.T) {
	const old, next = "unix:///old/secret-ish.sock", "unix:///new/secret-ish.sock"
	p := testPaths(t)
	writeConfig(t, p, "[env]\nDOCKER_HOST = \""+old+"\"\nGONE = \"was-here-value\"\n")
	spec := specFile(t, "[env]\nDOCKER_HOST = \""+next+"\"\nBASH_MAX_TIMEOUT_MS = \"1800000\"\n")
	got, err := Apply(context.Background(), p, spec)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, c := range got.Changes {
		lines = append(lines, c.String())
	}
	report := strings.Join(lines, "\n")
	for _, want := range []string{"env.BASH_MAX_TIMEOUT_MS set (was unset)", "env.DOCKER_HOST changed", "env.GONE removed"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not say %q:\n%s", want, report)
		}
	}
	for _, v := range []string{old, next, "1800000", "was-here-value"} {
		if strings.Contains(report, v) {
			t.Errorf("the report carries a value")
		}
	}
	for _, c := range got.Changes {
		for _, v := range []string{c.From, c.To} {
			if v != "" && v != hiddenValue {
				t.Errorf("%s carries a value in From or To", c.Key)
			}
		}
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Env["DOCKER_HOST"] != next || c.Env["BASH_MAX_TIMEOUT_MS"] != "1800000" || c.Env["GONE"] != "" {
		t.Error("config.toml's [env] is not the spec's")
	}
	// Applying the same spec again changes nothing.
	if again, err := Apply(context.Background(), p, spec); err != nil || again.Changed() {
		t.Errorf("a second apply changed %d settings (%v)", len(again.Changes), err)
	}
}
