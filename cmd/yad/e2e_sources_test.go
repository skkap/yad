package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/config"
)

// Decision 0088 end to end: yad hub lists the repositories its runs name, the
// daemon checks each with its own git and answers, the hub keeps the answers
// and shows them, and `yad status` shows them on the machine. One repository
// is on disk inside the owner's root and answers reachable; the other is a
// port nothing listens on, which answers network.
func TestE2EARunnerAnswersWhichRepositoriesItCanReach(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
	} {
		t.Setenv(k, v)
	}
	m := newMachine(t, claudeE2E)
	if err := os.Symlink(git, filepath.Join(os.Getenv("PATH"), "git")); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bare := filepath.Join(root, "acme.git")
	work := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", "--initial-branch=main", bare},
		{"-C", work, "init", "--quiet", "--initial-branch=main"},
		{"-C", work, "commit", "--quiet", "--allow-empty", "-m", "initial"},
		{"-C", work, "push", "--quiet", bare, "main"},
	} {
		if out, err := exec.Command(git, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	paths := config.Paths{Config: m.p.config, Data: m.p.data}
	cfg, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workdirs.Roots = []string{root}
	if err := config.Save(paths, cfg); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := "https://" + l.Addr().String() + "/acme/gone.git"
	l.Close()

	// For a harness this machine does not drive, so neither run is offered:
	// they are here for the repositories they name.
	for i, url := range []string{"file://" + bare, gone} {
		m.ok("hub", "submit", "--hub", m.service, "--harness", "nobody-drives-this", "--model", "m",
			"--run-id", "probe-"+string(rune('a'+i)), "--git", url, "look")
	}
	d := m.daemon()

	eventually(t, "the hub holds both answers", func() bool {
		runners, err := m.client().Runners(context.Background())
		return err == nil && len(runners) == 1 && len(runners[0].SourceReach) == 2
	})
	runners, _ := m.client().Runners(context.Background())
	for _, sr := range runners[0].SourceReach {
		switch sr.URL {
		case "file://" + bare:
			if !sr.Reachable || sr.Error != "" {
				t.Errorf("the repository on disk = %+v, want reachable", sr)
			}
		case gone:
			if sr.Reachable || sr.Error != "network" {
				t.Errorf("the closed port = %+v, want unreachable, network", sr)
			}
		default:
			t.Errorf("an answer for %q, which no run names", sr.URL)
		}
	}
	out := m.ok("hub", "runners", "--hub", m.service)
	if !strings.Contains(out, "repositories: 1 of 2 reachable") || !strings.Contains(out, gone+" — network") {
		t.Errorf("`yad hub runners`:\n%s", out)
	}
	status := m.ok("status")
	for _, want := range []string{"file://" + bare + ": reachable", gone + ": unreachable (network)"} {
		if !strings.Contains(status, want) {
			t.Errorf("`yad status` lacks %q:\n%s\ndaemon:\n%s", want, status, d.out.String())
		}
	}
}
