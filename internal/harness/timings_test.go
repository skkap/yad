package harness

import (
	"os"
	"testing"
	"time"
)

// answerBudget is what every version probe in this package may take unless a
// test shortens it on purpose. It is a bound on a broken build, not a budget
// for a busy one: each fake here is a script written fresh for its test, and
// macOS assesses a new executable on its first run — a tenth of a second on an
// idle machine, and past the shipped five seconds with the race-instrumented
// suite around it, which is how a probe meant to read a canned answer read a
// timeout instead (DEV-100). A probe that really hangs where the test meant it
// to answer still fails its test, only later.
const answerBudget = 30 * time.Second

func TestMain(m *testing.M) {
	VersionTimeoutForTests = answerBudget
	os.Exit(m.Run())
}

// shorten bounds the version probe for the length of a test. Only a probe that
// is meant to hang is shortened: a short bound on one meant to answer is the
// flake this package's neighbours had.
func shorten(t *testing.T, d time.Duration) {
	t.Helper()
	old := VersionTimeoutForTests
	VersionTimeoutForTests = d
	t.Cleanup(func() { VersionTimeoutForTests = old })
}

// What a shipped yad waits for `--version`, asserted as the value rather than
// by waiting it out: every test in this package runs under answerBudget. That
// no shipped file sets the seam is TestOnlyTestsReachTheProbeTimeouts, in
// internal/hostool, which walks the module for this name and its own.
func TestTheShippedVersionTimeoutIsFiveSeconds(t *testing.T) {
	if versionTimeout != 5*time.Second {
		t.Errorf("versionTimeout = %s, want 5s", versionTimeout)
	}
}
