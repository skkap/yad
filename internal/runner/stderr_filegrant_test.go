package runner

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/config"
)

// fileGrantSecret is a file grant's value: a credential the hub hands the run
// as a 0600 file whose path is the variable, not the value itself.
const fileGrantSecret = "sk-test-file-grant-value-0123456789"

// inEnvironment is what the fake says when the file grant's value is in its
// environment, which it never may be.
const inEnvironment = "the file grant's value is in my environment"

// fakeClaudeQuotesItsGrantFile is claude refusing the credential it was handed
// in a file, quoting it on stderr as it would one from the environment.
// "stderr-file-grant" then exits before its first frame; "silent-file-grant"
// says nothing more, as a harness that hangs before its first frame.
func fakeClaudeQuotesItsGrantFile(mode string) int {
	b, err := os.ReadFile(os.Getenv("DEPLOY_KEY_FILE"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "no grant file:", err)
		return 1
	}
	for _, kv := range os.Environ() {
		if strings.Contains(kv, fileGrantSecret) {
			fmt.Fprintln(os.Stderr, inEnvironment)
		}
	}
	fmt.Fprintf(os.Stderr, "the credential %s was refused\n", strings.TrimSpace(string(b)))
	if mode == "silent-file-grant" {
		time.Sleep(time.Hour)
	}
	return 1
}

// A grant delivered as a file is as much a credential yad handed the harness
// as one delivered in the environment (decision 0075), and the rule that a
// token is never transmitted holds even toward the hub that sent it (0064):
// its value is masked from the stderr a run's error quotes, on the harness's
// exit and on a watchdog's stop alike.
func TestAFileGrantsValueIsRedactedFromTheQuotedStderr(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		class string
	}{
		{"stderr-file-grant", adapter.ClassHarnessExited},
		{"silent-file-grant", ClassHarnessSilent},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			run := testRun("a", "s1")
			// With the newline a file usually ends in, which the harness
			// does not print.
			run.Grants = []v1.Grant{{Name: "DEPLOY_KEY_FILE", Value: fileGrantSecret + "\n", As: v1.GrantFile}}
			e.enqueue(t, run)
			x := claudeExecutor(t, e, tc.mode)
			// Long enough for a race-enabled child to have written its stderr.
			x.Config.Supervise.FirstOutput = config.Duration{Duration: 2 * time.Second}
			claimAndRun(t, l, x)
			res, ok := outboxResult(t, e, "a")
			if !ok || res.Error == nil || res.Error.Class != tc.class {
				t.Fatalf("result = %+v (error %+v), %v", res, res.Error, ok)
			}
			msg := res.Error.Message
			if want := "the credential " + adapter.Redacted + " was refused"; !strings.Contains(msg, want) {
				t.Errorf("message %q lacks %q", msg, want)
			}
			if strings.Contains(msg, fileGrantSecret) {
				t.Errorf("the run's error, which goes to the hub, carries the file grant's value: %q", msg)
			}
			if strings.Contains(msg, inEnvironment) {
				t.Errorf("the file grant's value reached the harness's environment: %q", msg)
			}
		})
	}
}
