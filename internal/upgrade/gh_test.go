package upgrade

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GH is the one place that leaves the process, so it is tested against a `gh`
// on PATH that is a shell script — the same trick the adapters use for their
// harnesses (ARCHITECTURE.md §7). What is worth proving is the argv: a wrong
// --pattern or a missing --repo is a failure only a real release would show.
const ghSeamStub = `#!/bin/sh
echo "$@" >>"$GH_LOG"
if [ -n "$GH_FAIL" ]; then echo "$GH_FAIL" >&2; exit 1; fi
case "$1 $2" in
  "release view") echo "$GH_TAG" ;;
  "release download")
    dir=
    while [ $# -gt 0 ]; do
      if [ "$1" = "--dir" ]; then dir=$2; fi
      shift
    done
    echo downloaded >"$dir/yad-linux-amd64"
    ;;
esac
`

// fakeGH puts the stub on PATH and answers with what a test sets.
func fakeGH(t *testing.T, env map[string]string) string {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(ghSeamStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	log := filepath.Join(t.TempDir(), "gh.log")
	t.Setenv("GH_LOG", log)
	for k, v := range env {
		t.Setenv(k, v)
	}
	return log
}

func TestGHLatest(t *testing.T) {
	log := fakeGH(t, map[string]string{"GH_TAG": "v0.4.0"})

	tag, err := GH{}.Latest(context.Background())
	if err != nil || tag != "v0.4.0" {
		t.Fatalf("Latest = %q, %v, want v0.4.0", tag, err)
	}
	calls := read(t, log)
	if !strings.Contains(calls, "--repo "+DefaultRepo) {
		t.Errorf("gh was called as %q, without the repository", calls)
	}
}

func TestGHLatestSaysWhenThereAreNoReleases(t *testing.T) {
	// gh's own words when a repository has published nothing. An operator sent
	// to `gh auth status` here would be debugging a login that works.
	fakeGH(t, map[string]string{"GH_FAIL": "release not found"})

	_, err := GH{}.Latest(context.Background())
	if err == nil {
		t.Fatal("Latest invented a release")
	}
	if !strings.Contains(err.Error(), "published no release yet") {
		t.Errorf("error %q, want it to say the repository has no releases", err)
	}
	if strings.Contains(err.Error(), "gh auth status") {
		t.Errorf("error %q sends the operator to check a login that is fine", err)
	}
}

func TestGHLatestReportsWhatGHSaid(t *testing.T) {
	fakeGH(t, map[string]string{"GH_FAIL": "gh: Could not resolve to a Repository"})

	_, err := GH{}.Latest(context.Background())
	if err == nil {
		t.Fatal("Latest reported success on a repository gh could not read")
	}
	if !strings.Contains(err.Error(), "Could not resolve") || !strings.Contains(err.Error(), "gh auth status") {
		t.Errorf("error %q, want gh's own words and the login to check", err)
	}
}

// The same asymmetry Latest had: gh fails identically for a missing tag and a
// missing login, and only one of those is fixed by logging in again.
func TestGHDownloadSaysWhenTheTagIsMissing(t *testing.T) {
	fakeGH(t, map[string]string{"GH_FAIL": "release not found"})

	err := GH{}.Download(context.Background(), "v9.9.9", []string{"yad-linux-amd64"}, t.TempDir())
	if err == nil {
		t.Fatal("Download reported success for a release that is not there")
	}
	if !strings.Contains(err.Error(), "has no release v9.9.9") {
		t.Errorf("error %q, want it to name the tag as the problem", err)
	}
	if strings.Contains(err.Error(), "gh auth status") {
		t.Errorf("error %q sends the operator to check a login that is fine", err)
	}
}

func TestGHDownloadAsksForEachAsset(t *testing.T) {
	log := fakeGH(t, nil)
	dir := t.TempDir()

	if err := (GH{Repo: "someone/fork"}).Download(context.Background(), "v0.4.0", []string{"yad-linux-amd64", ChecksumsName}, dir); err != nil {
		t.Fatalf("Download: %v", err)
	}
	calls := read(t, log)
	for _, want := range []string{
		"release download v0.4.0",
		"--repo someone/fork",
		"--dir " + dir,
		"--pattern yad-linux-amd64",
		"--pattern " + ChecksumsName,
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("gh was called as %q, want it to carry %q", calls, want)
		}
	}
}

func TestGHSaysWhenGHIsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := GH{}.Latest(context.Background())
	if err == nil {
		t.Fatal("Latest reported success with no gh to run")
	}
	if !strings.Contains(err.Error(), "gh auth login") || !strings.Contains(err.Error(), "private") {
		t.Errorf("error %q does not say what to install and why", err)
	}
}
