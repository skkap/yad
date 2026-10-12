package codex

import (
	"context"
	"os"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
)

// What yad handed Codex, which its errors may quote back: a value in its
// environment and what a file grant holds, which is in no variable.
const (
	envToken     = "tok-env-0123456789abcdef"
	grantContent = "grant-file-content-42"
)

var (
	said   = "the login with " + envToken + " and " + grantContent + " was refused"
	masked = "the login with " + adapter.Redacted + " and " + adapter.Redacted + " was refused"
)

// Codex's words in a run's error — a refused request, a resume of nothing, a
// failed turn — are masked of the run's environment and file grants, as its
// stderr is (DEV-192): the error goes to a hub.
func TestCodexsErrorIsMaskedInTheRunsError(t *testing.T) {
	const missing = "01a0b86e-0000-7000-8000-000000000000"
	refuse := func(id string) string {
		return `{"error":{"code":-32600,"message":"` + said + `"},"id":` + id + `}`
	}
	for _, tc := range []struct {
		name    string
		fixture func(t *testing.T) string
		resume  string
		class   string
		says    []string
	}{
		{name: "a refused request", fixture: func(t *testing.T) string {
			return answerBefore(t, "plain", `{"method":"remoteControl/status/changed"`, refuse("2"))
		}, class: adapter.ClassHarness, says: []string{"codex refused thread/start: " + masked}},
		{name: "a refused turn", fixture: func(t *testing.T) string {
			return answerBefore(t, "plain", `{"id":3,"result"`, refuse("3"))
		}, class: adapter.ClassHarness, says: []string{"codex refused the turn: " + masked}},
		{name: "a resume of nothing", fixture: func(t *testing.T) string {
			return rewrite(t, "resume-missing", "for thread id "+missing, "for thread id "+missing+", "+said)
		}, resume: missing, class: adapter.ClassSessionNotFound,
			says: []string{"(no rollout found for thread id " + missing + ", " + masked + ")"}},
		{name: "a failed turn", fixture: func(t *testing.T) string {
			path := rewrite(t, "error", "gpt-nonexistent-9", said)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			return writeFixture(t, strings.ReplaceAll(string(b), `"additionalDetails":null`, `"additionalDetails":"`+grantContent+` again"`))
		}, class: adapter.ClassHarness,
			says: []string{"The '" + masked + "' model is not supported", "(" + adapter.Redacted + " again)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &harness{fixture: tc.fixture(t), env: map[string]string{"YAD_TEST_TOKEN": envToken}}
			spec := h.spec(t)
			spec.Secrets = []string{grantContent}
			spec.NativeSessionID = tc.resume
			evs, out, _ := drive(t, context.Background(), spec, nil)
			if out.State != v1.RunFailed || out.Error == nil || out.Error.Class != tc.class {
				t.Fatalf("outcome = %+v (%+v)", out, out.Error)
			}
			for _, want := range tc.says {
				if !strings.Contains(out.Error.Message, want) {
					t.Errorf("the run's error %q, want it to carry %q", out.Error.Message, want)
				}
			}
			all := []string{out.Error.Message}
			for _, e := range kinds(evs, v1.EventError) {
				all = append(all, e.Error.Message)
			}
			for _, s := range all {
				if strings.Contains(s, envToken) || strings.Contains(s, grantContent) {
					t.Errorf("a secret went out unmasked: %q", s)
				}
			}
		})
	}
}

// answerBefore writes a copy of a fixture that ends just before the line with
// this prefix, with answer in its place.
func answerBefore(t *testing.T, name, prefix, answer string) string {
	t.Helper()
	b, err := os.ReadFile(cutBefore(t, name, prefix))
	if err != nil {
		t.Fatal(err)
	}
	return writeFixture(t, string(b)+answer+"\n")
}

// rewrite writes a copy of a fixture with old replaced by new in every line
// Codex wrote; what the adapter sent is left as recorded.
func rewrite(t *testing.T, name, old, new string) string {
	t.Helper()
	b, err := os.ReadFile(fixture(name))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	n := 0
	for i, l := range lines {
		if !strings.HasPrefix(l, `{">"`) && strings.Contains(l, old) {
			lines[i] = strings.ReplaceAll(l, old, new)
			n++
		}
	}
	if n == 0 {
		t.Fatalf("%s has no %s in what Codex wrote", name, old)
	}
	return writeFixture(t, strings.Join(lines, "\n"))
}
