package config

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Absent and true take sources on the machine; only false refuses them.
func TestAllowsPathSources(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{"", true},
		{"[workdirs]\npath_sources = true\n", true},
		{"[workdirs]\npath_sources = false\n", false},
		{"[workdirs]\nroots = [\"/srv/src\"]\npath_sources = false\n", false},
	} {
		p := testPaths(t)
		writeConfig(t, p, tc.src)
		c, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Workdirs.AllowsPathSources(); got != tc.want {
			t.Errorf("%q: AllowsPathSources() = %v, want %v", tc.src, got, tc.want)
		}
	}
}

// path_sources = false is how an owner switches sources on the machine off,
// because `roots = []` does not survive a rewrite (DEV-72). So it must survive
// every writer of config.toml: `yad connect` adding a connection, an account
// added or removed at the terminal or by a hub, and `yad config apply` from a
// spec that says it. Unset stays unset, so the home default is never written.
func TestPathSourcesSurvivesEveryWriter(t *testing.T) {
	ctx := context.Background()
	const off = "[workdirs]\npath_sources = false\n"
	for _, w := range []struct {
		name  string
		write func(t *testing.T, p Paths) error
		// spec is a writer that takes [workdirs] from somewhere else, so
		// what an unset file becomes is not this test's to say.
		spec bool
	}{
		{name: "a connection added", write: func(_ *testing.T, p Paths) error {
			_, err := Update(ctx, p, func(c *Config) (bool, error) {
				c.Connections = append(c.Connections, Connection{Name: "home", URL: "https://hub.example.com/v1"})
				return true, nil
			})
			return err
		}},
		{name: "an account added", write: func(_ *testing.T, p Paths) error {
			_, err := UpdateAccounts(ctx, p, "claude", WithAccount("second"))
			return err
		}},
		{name: "an account removed", write: func(_ *testing.T, p Paths) error {
			_, err := UpdateAccounts(ctx, p, "claude", WithoutAccount("main"))
			return err
		}},
		{name: "a spec that says it applied", spec: true, write: func(t *testing.T, p Paths) error {
			_, err := Apply(ctx, p, specFile(t, "capacity = 3\n"+off))
			return err
		}},
	} {
		t.Run(w.name, func(t *testing.T) {
			p := testPaths(t)
			writeConfig(t, p, "[harness.claude]\naccounts = [\"main\"]\n"+off)
			if err := w.write(t, p); err != nil {
				t.Fatal(err)
			}
			c, err := Load(p)
			if err != nil {
				t.Fatal(err)
			}
			if c.Workdirs.AllowsPathSources() {
				b, _ := os.ReadFile(p.ConfigFile())
				t.Errorf("path_sources is on again after the write:\n%s", b)
			}
		})
		if w.spec {
			continue
		}
		t.Run(w.name+", unset", func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			p := testPaths(t)
			writeConfig(t, p, "[harness.claude]\naccounts = [\"main\"]\n")
			if err := w.write(t, p); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(p.ConfigFile())
			if strings.Contains(string(b), "path_sources") || strings.Contains(string(b), "roots") {
				t.Errorf("config.toml names a workdirs setting the owner never set:\n%s", b)
			}
		})
	}
}

// The spec decides [workdirs], path_sources with it: a spec that leaves it
// out switches it back on, and the change says so.
func TestApplyTakesPathSourcesFromTheSpec(t *testing.T) {
	p := testPaths(t)
	writeConfig(t, p, "[workdirs]\npath_sources = false\n")
	got, err := Apply(context.Background(), p, specFile(t, "capacity = 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Workdirs.PathSources != nil {
		t.Errorf("path_sources = %v after a spec without it, want unset", *c.Workdirs.PathSources)
	}
	var said bool
	for _, ch := range got.Changes {
		said = said || ch.String() == "workdirs.path_sources removed (was false)"
	}
	if !said {
		t.Errorf("changes %v do not say path_sources was removed", got.Changes)
	}
}
