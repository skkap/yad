package claude

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
)

// Claude's words in a failed result — its result text, or the errors it
// lists — are masked of the run's environment and file grants before a run's
// error quotes them, as its stderr is (YAD-192): the error goes to a hub, and
// a Claude whose login was refused may quote what it was refused.
func TestAFailedResultIsMaskedInTheRunsError(t *testing.T) {
	const envToken, grantContent = "tok-env-0123456789abcdef", "grant-file-content-42"
	said := "the login with " + envToken + " and " + grantContent + " was refused"
	masked := "the login with " + adapter.Redacted + " and " + adapter.Redacted + " was refused"
	for _, tc := range []struct {
		name  string
		set   map[string]any
		class string
	}{
		{"its result", map[string]any{"result": "API Error: 401 " + said, "terminal_reason": "api_error"}, adapter.ClassHarness},
		{"its errors", map[string]any{"result": "", "errors": []string{"No conversation found with session ID: s0", said}}, adapter.ClassSessionNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := derive(t, "error", func(lines []string) []string { return editResult(t, lines, tc.set) })
			h := &harness{fixture: path, env: map[string]string{"YAD_TEST_TOKEN": envToken}}
			spec := h.spec(t)
			spec.Secrets = []string{grantContent}
			evs, out, _ := drive(t, context.Background(), spec, nil)
			if out.State != v1.RunFailed || out.Error == nil || out.Error.Class != tc.class {
				t.Fatalf("outcome %+v (%+v)", out, out.Error)
			}
			if !strings.Contains(out.Error.Message, masked) {
				t.Errorf("the run's error %q, want it to carry %q", out.Error.Message, masked)
			}
			all := []string{out.Error.Message}
			for _, e := range evs {
				if e.Error != nil {
					all = append(all, e.Error.Message)
				}
			}
			for _, s := range all {
				if strings.Contains(s, envToken) || strings.Contains(s, grantContent) {
					t.Errorf("a secret went out unmasked: %q", s)
				}
			}
		})
	}
}
