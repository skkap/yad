package claude

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/supervise"
)

// modelsFixture is the list_models answer recorded from a real login.
const modelsFixture = "testdata/claude-2.1.284/list-models.jsonl"

// listed is what the recorded login offered, in Claude's order.
var listed = []string{"default", "opus", "claude-fable-5-1", "sonnet", "haiku", "claude-sonnet-5", "claude-opus-5",
	"claude-fable-5", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "claude-sonnet-4-6"}

func list(t *testing.T, fixture string, env map[string]string, timeout time.Duration) ([]string, seen, error) {
	t.Helper()
	p, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{fixture: p, env: env}
	spec := h.spec(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	got, err := ListModels(ctx, spec.Binary, spec.Workdir, spec.Env)
	return got, h.seen(t), err
}

// The list is list_models's answer, with no user message sent: nothing is
// run, so nothing is spent (DEV-50). No session is kept and no hook fires.
func TestListModelsAsksClaude(t *testing.T) {
	got, s, err := list(t, modelsFixture, nil, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, listed) {
		t.Errorf("models = %v, want %v", got, listed)
	}
	if n := len(s.frames("user")); n != 0 {
		t.Errorf("%d user frames sent: listing models must take no turn", n)
	}
	reqs := s.frames("control_request")
	if len(reqs) != 1 || reqs[0]["request"].(map[string]any)["subtype"] != "list_models" {
		t.Errorf("control requests = %v, want one list_models", reqs)
	}
	if _, ok := s.flag("--no-session-persistence"); !ok {
		t.Errorf("argv %v keeps a session", s.argv)
	}
	if v, _ := s.flag("--settings"); v != `{"disableAllHooks":true}` {
		t.Errorf("argv %v runs the owner's hooks", s.argv)
	}
	for _, f := range []string{"--session-id", "--resume", "--model"} {
		if _, ok := s.flag(f); ok {
			t.Errorf("argv %v carries %s", s.argv, f)
		}
	}
}

// fixtureOf writes a stream for the fake to play.
func fixtureOf(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "list.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func answer(models string) string {
	return `{"type":"control_response","response":{"subtype":"success","request_id":"yad-list-models","response":{"models":[` + models + `]}}}`
}

// What Claude writes before the answer is read past. A model it lists as
// disabled is one this login will not run, and stays out; so does anything
// not shaped like a model name, since the document reaches every hub (DEV-67).
func TestListModelsKeepsOnlyWhatTheLoginRuns(t *testing.T) {
	path := fixtureOf(t,
		`{"type":"system","subtype":"hook_started","hook_name":"SessionStart:startup"}`,
		answer(`{"value":"opus[1m]"},{"value":"claude-sonnet-5-5","disabled":true},{"value":"/Users/someone/.claude"},`+
			`{"value":"a sentence someone wrote"},{"value":"opus[1m]"},{"value":"haiku"}`),
	)
	got, _, err := list(t, path, nil, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"opus[1m]", "haiku"}; !slices.Equal(got, want) {
		t.Errorf("models = %v, want %v", got, want)
	}
}

// A Claude that cannot answer is an error, in yad's words; the caller reports
// its own fallback instead, and nothing Claude said travels.
//
// One that died before answering is one error, whether it was gone before
// the request was written or went after reading it, so the capability
// document and doctor say the same thing whichever order the OS picked
// (DEV-149). "died" leaves the order to the OS; the two after it fix it, and
// check that the order they name is the one that happened.
func TestListModelsFailures(t *testing.T) {
	const died = "claude ended its output without listing its models"
	startup := []string{`{"type":"system","subtype":"init"}`}
	for _, tc := range []struct {
		name  string
		lines []string
		env   map[string]string
		want  string
		// exitFirst holds the request until Claude has exited.
		exitFirst bool
		// asked is how many list_models requests Claude read; -1 is either.
		asked int
	}{
		{"refused", []string{`{"type":"control_response","response":{"subtype":"error","request_id":"yad-list-models","error":"Unsupported control request subtype: list_models at /Users/someone"}}`},
			nil, "refused list_models", false, 1},
		{"nothing listed", []string{answer(`{"value":"claude-x","disabled":true}`)}, nil, "listed no models", false, 1},
		{"died", startup, map[string]string{"CLAUDE_TEST_MODE": "died"}, died, false, -1},
		{"died before it was asked", startup, map[string]string{"CLAUDE_TEST_MODE": "died"}, died, true, 0},
		{"died when asked", startup, map[string]string{"CLAUDE_TEST_MODE": "died-when-asked"}, died, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.exitFirst {
				beforeModelsAsked = func(p *supervise.Process) { <-p.Done() }
				t.Cleanup(func() { beforeModelsAsked = nil })
			}
			got, s, err := list(t, fixtureOf(t, tc.lines...), tc.env, 20*time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("models %v, err %v; want an error saying %q", got, err, tc.want)
			}
			if tc.want == died && err.Error() != died {
				t.Errorf("err %q, want exactly %q", err, died)
			}
			if strings.Contains(err.Error(), "someone") {
				t.Errorf("the error quotes Claude: %v", err)
			}
			if n := len(s.frames("control_request")); tc.asked >= 0 && n != tc.asked {
				t.Errorf("claude read %d list_models requests, want %d", n, tc.asked)
			}
		})
	}
}

// One that never answers is given up on when the caller's context ends, and
// stopped even when it ignores SIGTERM.
func TestListModelsDoesNotWaitOnAWedgedClaude(t *testing.T) {
	oldExit, oldTerm := modelsExitGrace, termGrace
	modelsExitGrace, termGrace = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { modelsExitGrace, termGrace = oldExit, oldTerm })
	start := time.Now()
	_, _, err := list(t, fixtureOf(t, `{"type":"system","subtype":"init"}`), map[string]string{"CLAUDE_TEST_MODE": "linger"}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "in time") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if took := time.Since(start); took > drainGrace+5*time.Second {
		t.Errorf("gave up after %s", took)
	}
}
