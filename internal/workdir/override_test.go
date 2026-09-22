//go:build unix

package workdir

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// YAD_GIT_PATH is for a runner whose PATH has no git, and the capability
// document advertises the git it names. So that is the git a git source is
// prepared with, and the one the setup hook finds by name — even though the
// file is not called git (DEV-107, 0045).
func TestAGitOnlyTheOverrideNamesPreparesTheWorkdir(t *testing.T) {
	f := newFixture(t)
	hook := "#!/bin/sh\ngit --version > \"$WT_ROOT/hook-git\"\n"
	o := newOrigin(t, f.root, "acme", map[string]string{hookPath: hook, ".gitignore": "hook-git\n"})
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	override := filepath.Join(t.TempDir(), "git-2.45")
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(override, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	for _, v := range []string{"YAD_GH_PATH", "YAD_DOCKER_PATH"} {
		t.Setenv(v, "")
	}
	t.Setenv("YAD_GIT_PATH", override)

	p, _, err := f.prepare("s1", gitSource(o.bare, "main", "feat/a"))
	if err != nil {
		t.Fatalf("a runner whose only git is YAD_GIT_PATH's could not prepare a git source: %v", err)
	}
	if got := read(t, filepath.Join(p.Dir, "hook-git")); !strings.HasPrefix(got, "git version") {
		t.Errorf("the setup hook's git said %q", got)
	}
	log := read(t, calls)
	for _, verb := range []string{"fetch", "worktree", "--version"} {
		if !slices.Contains(strings.Fields(log), verb) {
			t.Errorf("the override's git never ran %s; it ran:\n%s", verb, log)
		}
	}
}
