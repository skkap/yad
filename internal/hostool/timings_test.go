package hostool

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// answerBudget is what every probe in this package may take unless a test
// shortens it on purpose. It is a bound on a broken build, not a budget for a
// busy one: each fake here is a script written fresh for its test, and macOS
// assesses a new executable on its first run — a tenth of a second on an idle
// machine, and past the shipped five seconds with the race-instrumented suite
// around it, which is how a probe meant to read a canned answer read a timeout
// instead (DEV-100). A probe that really hangs where the test meant it to
// answer still fails its test, only later.
const answerBudget = 30 * time.Second

func TestMain(m *testing.M) {
	VersionTimeoutForTests, StatusTimeoutForTests = answerBudget, answerBudget
	os.Exit(m.Run())
}

// shorten bounds one probe for the length of a test, the other keeping the
// budget TestMain gave it. Only a probe that is meant to hang is shortened: a
// short bound on one meant to answer is the flake this package had.
func shorten(t *testing.T, seam *time.Duration, d time.Duration) {
	t.Helper()
	old := *seam
	*seam = d
	t.Cleanup(func() { *seam = old })
}

// What a shipped yad waits for a host tool, asserted as the value rather than
// by waiting it out: every test in this package runs under answerBudget.
func TestTheShippedProbeTimeoutsAreFiveSeconds(t *testing.T) {
	if versionTimeout != 5*time.Second || statusTimeout != 5*time.Second {
		t.Errorf("versionTimeout, statusTimeout = %s, %s; want 5s each", versionTimeout, statusTimeout)
	}
}

// The seams exist so a test can wait longer than a shipped runner would, and a
// shipped runner that waited that long would hold every capability probe on a
// hung tool for as long. The check is the source: any shipped file of the
// module that assigns one of them. internal/harness declares its own
// VersionTimeoutForTests, and this walks the whole module by name, so its
// shipped files are covered here too.
func TestOnlyTestsReachTheProbeTimeouts(t *testing.T) {
	for _, name := range []string{"VersionTimeoutForTests", "StatusTimeoutForTests"} {
		assertSeamIsNeverAssignedOutsideTests(t, name)
	}
}

// assertSeamIsNeverAssignedOutsideTests fails when any shipped file of the
// module assigns name or takes its address. Reading it is what the probe does;
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
			t.Errorf("%s:%d assigns %s; only a test may change it", path, line, name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
