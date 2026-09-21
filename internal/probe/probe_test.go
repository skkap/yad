package probe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// The rule for an override, in every shape a path can take. What each outcome
// does to a harness and a host tool is internal/capability's
// TestAnOverrideMeansTheSameForHarnessesAndHostTools; this is only which path
// wins and what is said about it.
func TestFind(t *testing.T) {
	for _, tc := range []struct {
		name string
		// override builds YAD_TOOL_PATH's value in dir; nil leaves it unset.
		override func(t *testing.T, dir string) string
		onPATH   bool
		// want is which binary is used: "override", "path" or "" for none.
		want                   string
		wantWarning, wantError bool
	}{
		{name: "no override, on PATH", onPATH: true, want: "path"},
		{name: "no override, nowhere"},
		{name: "override names a file", onPATH: true, want: "override",
			override: func(t *testing.T, dir string) string { return file(t, dir, 0o755) }},
		// Taken as the binary: whether it runs is the version probe's to find.
		{name: "override names a file that is not executable", onPATH: true, want: "override",
			override: func(t *testing.T, dir string) string { return file(t, dir, 0o644) }},
		{name: "override names nothing, on PATH", onPATH: true, want: "path", wantWarning: true,
			override: func(t *testing.T, dir string) string { return filepath.Join(dir, "gone") }},
		{name: "override names nothing, nowhere", wantError: true,
			override: func(t *testing.T, dir string) string { return filepath.Join(dir, "gone") }},
		{name: "override is a directory, on PATH", onPATH: true, want: "path", wantWarning: true,
			override: func(t *testing.T, dir string) string { return dir }},
		{name: "override is a directory, nowhere", wantError: true,
			override: func(t *testing.T, dir string) string { return dir }},
		{name: "override runs through a file", onPATH: true, want: "path", wantWarning: true,
			override: func(t *testing.T, dir string) string { return filepath.Join(file(t, dir, 0o755), "tool") }},
		{name: "override is a link to nothing", onPATH: true, want: "path", wantWarning: true,
			override: func(t *testing.T, dir string) string {
				link := filepath.Join(dir, "link")
				if err := os.Symlink(filepath.Join(dir, "gone"), link); err != nil {
					t.Fatal(err)
				}
				return link
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pathDir, own := t.TempDir(), t.TempDir()
			onPATH := filepath.Join(pathDir, "tool")
			if tc.onPATH {
				if err := os.WriteFile(onPATH, []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", pathDir)
			override := ""
			if tc.override != nil {
				override = tc.override(t, own)
			}
			t.Setenv("YAD_TOOL_PATH", override)

			f := Find("YAD_TOOL_PATH", "tool", []string{"--version"})
			want := map[string]string{"override": override, "path": onPATH}[tc.want]
			if f.Path != want || f.FromOverride != (tc.want == "override") {
				t.Errorf("Path %q, from the override %v; want %q", f.Path, f.FromOverride, want)
			}
			if (f.Warning != "") != tc.wantWarning || (f.Error != "") != tc.wantError {
				t.Errorf("Warning %q, Error %q; want a warning %v, an error %v", f.Warning, f.Error, tc.wantWarning, tc.wantError)
			}
			for _, said := range []string{f.Warning, f.Error} {
				if said == "" {
					continue
				}
				if !strings.Contains(said, "YAD_TOOL_PATH") || !strings.Contains(said, "unset it") {
					t.Errorf("%q, want the variable and the next action", said)
				}
				for _, leak := range []string{own, pathDir} {
					if strings.Contains(said, leak) {
						t.Errorf("%q names a path on the machine", said)
					}
				}
			}
		})
	}
}

// The start failure sends its reader to the thing still worth checking: the
// override when it came from one, the binary itself when PATH proved it
// executable.
func TestWontStartNamesWhereTheBinaryCameFrom(t *testing.T) {
	fromEnv := Found{FromOverride: true, envVar: "YAD_TOOL_PATH", name: "tool", versionArgs: []string{"--version"}}
	if got := fromEnv.WontStart(); !strings.Contains(got, "YAD_TOOL_PATH") || !strings.Contains(got, "point it at an executable tool") {
		t.Errorf("from the override: %q", got)
	}
	fromPATH := Found{envVar: "YAD_TOOL_PATH", name: "tool", versionArgs: []string{"--version"}}
	got := fromPATH.WontStart()
	if !strings.Contains(got, "run `tool --version` on this machine") || strings.Contains(got, "YAD_TOOL_PATH") {
		t.Errorf("from PATH: %q", got)
	}
}

func file(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, "own-tool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// Every command a message sets apart is pasted, so it must run as printed —
// proved by a real sh, with an argument no catalog entry has yet but nothing
// stops one having (AGENTS.md).
func TestCommandsRunAsPrinted(t *testing.T) {
	f := Found{envVar: "YAD_TOOL_PATH", name: "tool", versionArgs: []string{"--version", "a b'c"}}
	for _, msg := range []string{f.WontStart(), NoAnswer(f.Command(), time.Second), WontAnswer(f.Command())} {
		cmds := shellwordtest.Commands(msg, "tool ")
		if len(cmds) != 1 {
			t.Fatalf("%q sets apart %d commands, want 1", msg, len(cmds))
		}
		shellwordtest.Check(t, cmds[0], "tool", "--version", "a b'c")
	}
}
