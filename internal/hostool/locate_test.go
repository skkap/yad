package hostool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Links is how a run's children reach the tools detection resolved: a link
// named for each tool only its override finds, first on PATH, and nothing for
// a tool PATH already has (0045). Each case is one step on the same runner,
// so a link left from the step before must follow the override or go.
func TestLinksFollowDetection(t *testing.T) {
	onPATH := t.TempDir()
	for _, name := range []string{"gh", "docker"} {
		if err := os.WriteFile(filepath.Join(onPATH, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	elsewhere := t.TempDir()
	gitOverride := filepath.Join(elsewhere, "git-2.45")
	ghOverride := filepath.Join(elsewhere, "gh-beta")
	for _, p := range []string{gitOverride, ghOverride} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", onPATH)
	dir := LinksDir(t.TempDir())

	for _, step := range []struct {
		name      string
		overrides map[string]string
		links     map[string]string // link name → target; absent: no link
	}{
		{"no override", map[string]string{}, map[string]string{}},
		{"overrides outside PATH, under other names",
			map[string]string{"YAD_GIT_PATH": gitOverride, "YAD_GH_PATH": ghOverride},
			map[string]string{"git": gitOverride, "gh": ghOverride}},
		{"one override gone, one naming nothing",
			map[string]string{"YAD_GH_PATH": filepath.Join(elsewhere, "uninstalled")},
			map[string]string{}},
		{"an override given relative to the runner's directory",
			map[string]string{"YAD_GIT_PATH": relative(t, gitOverride)},
			map[string]string{"git": gitOverride}},
	} {
		t.Run(step.name, func(t *testing.T) {
			for _, tool := range Catalog() {
				t.Setenv(tool.EnvPath, step.overrides[tool.EnvPath])
			}
			path, err := Links(dir)
			if err != nil {
				t.Fatal(err)
			}
			switch want := "PATH=" + dir + ":" + onPATH; {
			case len(step.links) == 0 && path != "":
				t.Errorf("Links = %q with no tool only an override finds, want the runner's own PATH", path)
			case len(step.links) > 0 && path != want:
				t.Errorf("Links = %q, want %q", path, want)
			}
			for _, tool := range Catalog() {
				got, err := os.Readlink(filepath.Join(dir, tool.Binary))
				want, linked := step.links[tool.Binary]
				switch {
				case !linked && err == nil:
					t.Errorf("%s links to %s, want no link: PATH's copy, or none, is detection's", tool.Binary, got)
				case linked && got != want:
					t.Errorf("%s links to %q (%v), want %q", tool.Binary, got, err, want)
				}
			}
		})
	}
}

// relative is path from the test's working directory, as an owner who typed
// YAD_GIT_PATH=../bin/git would have it.
func relative(t *testing.T, path string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, path)
	if err != nil || strings.HasPrefix(rel, "/") {
		t.Skipf("no relative path from %s to %s", wd, path)
	}
	return rel
}

// Locate is detection's answer, so the git workdir runs is the one the
// capability document advertised: the override's, then PATH's once it names
// nothing (0044).
func TestLocateIsDetection(t *testing.T) {
	onPATH := t.TempDir()
	pathGit := filepath.Join(onPATH, "git")
	override := filepath.Join(t.TempDir(), "git-2.45")
	for _, p := range []string{pathGit, override} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", onPATH)
	for _, c := range []struct{ override, want string }{
		{"", pathGit},
		{override, override},
		{filepath.Join(t.TempDir(), "gone"), pathGit},
	} {
		t.Setenv("YAD_GIT_PATH", c.override)
		if got, ok := Locate("git"); !ok || got != c.want {
			t.Errorf("YAD_GIT_PATH=%q: Locate = %q, %v; want %q", c.override, got, ok, c.want)
		}
	}
}
