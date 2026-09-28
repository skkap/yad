package claude

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/probe"
)

// found is detection's answer for the test binary playing claude, through the
// override, as a runner configured with a path would have it.
func found(t *testing.T) probe.Found {
	t.Helper()
	t.Setenv("YAD_TEST_CLAUDE_HELP_PATH", os.Args[0])
	f := probe.Find("YAD_TEST_CLAUDE_HELP_PATH", "claude", []string{"--version"})
	if f.Path == "" {
		t.Fatal("the test binary was not found through the override")
	}
	return f
}

// A Claude that does not know a flag every run passes would claim every run
// and fail each at its arguments; the probe reports it unable to take runs,
// saying to upgrade it, and a current one passes (decision 0050).
func TestFlagsCheck(t *testing.T) {
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	for _, tc := range []struct {
		mode, wantErr, wantWarning string
	}{
		{mode: "current"},
		{mode: "old", wantErr: `does not know --system-prompt-snapshot, which every run passes — run ` + "`" + `"$YAD_TEST_CLAUDE_HELP_PATH" update` + "`"},
		{mode: "nofork", wantErr: `does not know --fork-session, which a run opening a fork passes — run ` + "`" + `"$YAD_TEST_CLAUDE_HELP_PATH" update` + "`"},
		// Not asked is not refused: a probe that failed leaves the harness
		// drivable, and the adapter's own error is the backstop.
		{mode: "broken", wantWarning: "yad could not check which flags this claude knows"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv("CLAUDE_TEST_HELP", tc.mode)
			e, w := FlagsCheck(context.Background(), found(t), "2.1.280-"+tc.mode)
			if tc.wantErr == "" && e != "" || !strings.Contains(e, tc.wantErr) {
				t.Errorf("error = %q, want %q", e, tc.wantErr)
			}
			if tc.wantWarning == "" && w != "" || !strings.Contains(w, tc.wantWarning) {
				t.Errorf("warning = %q, want %q", w, tc.wantWarning)
			}
			for _, leak := range []string{"something went badly wrong", os.Args[0]} {
				if strings.Contains(e+w, leak) {
					t.Errorf("the answer quotes %q: %q / %q", leak, e, w)
				}
			}
		})
	}
}

// Asked on every capability probe, so a definite answer is kept for the
// binary and version, and a probe that could not run only for helpRetry.
func TestFlagsCheckIsRemembered(t *testing.T) {
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	clock := time.Now()
	defer func(f func() time.Time) { helpNow = f }(helpNow)
	helpNow = func() time.Time { return clock }
	f := found(t)

	t.Setenv("CLAUDE_TEST_HELP", "old")
	if e, _ := FlagsCheck(context.Background(), f, "remembered"); e == "" {
		t.Fatal("an old claude passed")
	}
	t.Setenv("CLAUDE_TEST_HELP", "current")
	if e, _ := FlagsCheck(context.Background(), f, "remembered"); e == "" {
		t.Error("a definite answer was asked again for the same binary and version")
	}
	if e, _ := FlagsCheck(context.Background(), f, "upgraded"); e != "" {
		t.Errorf("an upgraded claude still refused: %q", e)
	}

	t.Setenv("CLAUDE_TEST_HELP", "broken")
	if _, w := FlagsCheck(context.Background(), f, "retried"); w == "" {
		t.Fatal("a broken --help was not reported")
	}
	t.Setenv("CLAUDE_TEST_HELP", "current")
	if _, w := FlagsCheck(context.Background(), f, "retried"); w == "" {
		t.Error("a failed probe was not believed for helpRetry")
	}
	clock = clock.Add(helpRetry + time.Second)
	if e, w := FlagsCheck(context.Background(), f, "retried"); e != "" || w != "" {
		t.Errorf("after helpRetry: %q / %q, want the probe asked again and passed", e, w)
	}
}
