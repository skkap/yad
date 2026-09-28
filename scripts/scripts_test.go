// Package scripts holds no Go. It is where the shell scripts beside it are
// tested, from Go so that `go test ./...` runs them, the same way
// internal/upgrade tests scripts/install.sh.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo is a throwaway git repository holding nothing but empty commits and
// the tags a test gives it. It has no remote, so no tag made here can be
// pushed anywhere.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "master")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	// A developer's own git config must not decide what these repositories
	// look like: a signing hook or a tag.sort would change the answer.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) commit(msg string) string {
	r.t.Helper()
	r.git("commit", "-q", "--allow-empty", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// run runs a script from this directory with the repository as its working
// directory, returning its output and exit status.
func (r *repo) run(script string, args ...string) (string, int) {
	r.t.Helper()
	abs, err := filepath.Abs(script)
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{abs}, args...)...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatal(err)
	}
	return string(out), code
}
