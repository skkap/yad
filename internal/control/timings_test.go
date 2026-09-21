//go:build unix

package control

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// What a shipped yad waits for an answer, asserted beside the seam that lowers
// it for tests. Asserted as the value rather than by waiting it out, because
// the five seconds the seam exists to save are exactly what such a test would
// spend; what the value protects is described where it is declared.
func TestTheShippedAskTimeoutIsFiveSeconds(t *testing.T) {
	if AskTimeoutForTests != 0 {
		t.Fatalf("a test left the ask seam set to %s", AskTimeoutForTests)
	}
	if askTimeout != 5*time.Second {
		t.Errorf("askTimeout = %s, want 5s", askTimeout)
	}
}

// A shipped yad that gave up on the socket sooner would call a daemon merely
// busy with a sync wedged, and signal where it should have asked. The seam that makes
// the stop tests fast must not travel into the binary, and the check is the
// source: any shipped file that assigns it, this package's own included.
func TestOnlyTestsReachTheAskTimeout(t *testing.T) {
	assertSeamIsNeverAssignedOutsideTests(t, "AskTimeoutForTests")
}

// assertSeamIsNeverAssignedOutsideTests fails when any shipped file of the
// module assigns name or takes its address. Reading it is what the timeout does;
// writing it is what only a test may do, and the file that declares it is not
// exempt — a setter beside the declaration is exactly where someone would write
// one. Matching the identifier is a backstop, not a proof: a value reached some
// other way is beyond what reading the source can see.
func assertSeamIsNeverAssignedOutsideTests(t *testing.T, name string) {
	t.Helper()
	written := regexp.MustCompile(`&\s*` + name + `\b|\b` + name + `\s*(?:[-+*/%|&^]|<<|>>)?=[^=]`)
	const root = "../.."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata"):
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := written.FindIndex(raw); loc != nil {
			line := 1 + strings.Count(string(raw[:loc[0]]), "\n")
			t.Errorf("%s:%d assigns %s; only a test may shorten it", path, line, name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
