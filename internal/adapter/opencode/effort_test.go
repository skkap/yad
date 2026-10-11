package opencode

import (
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// effortModel is a free model on OpenCode Zen with effort levels: the one
// every effort fixture was recorded on (record_test.go).
const effortModel = "opencode/longcat-2.5-preview-free"

// efforts is every effort the adapter set, in order.
func efforts(s seen) []any {
	var out []any
	for _, m := range s.sent("session/set_config_option") {
		if params, _ := m["params"].(map[string]any); params["configId"] == "effort" {
			out = append(out, params["value"])
		}
	}
	return out
}

// A run without an effort that resumes a session an earlier run set an
// effort on must not run at that earlier run's effort. OpenCode 1.18.33
// restores the session's last variant on session/resume and prompts at it,
// so the adapter puts the session back on OpenCode's default first
// (decision 0049: absent is the harness's default on every run).
func TestAResumedRunWithoutAnEffortDoesNotInheritTheLastOne(t *testing.T) {
	p := &play{fixture: fixture(t, "resume-held-effort")}
	spec := p.spec(t)
	spec.NativeSessionID = recordedSession(t, p.fixture, "session/resume")
	spec.Model = effortModel
	spec.Effort = "" // the hub asked for the harness's default
	_, o := run(t, spec, nil)
	if o.State != v1.RunSucceeded {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}
	// The fixture answers the prompt only after the effort is set, so a
	// prompt sent first would not have succeeded.
	if got := efforts(p.seen(t)); len(got) != 1 || got[0] != defaultEffort {
		t.Errorf("effort set %v, want only %q before the prompt", got, defaultEffort)
	}
}

// The reset is for a session that holds another run's effort, and only for
// a run that asks for none: a fork copies the effort with the conversation;
// a session already at the default, or one this run opened, holds no other
// run's; and a run with an effort sets its own, once.
func TestTheDefaultEffortIsSetOnlyOverAnotherRunsEffort(t *testing.T) {
	for _, tc := range []struct {
		name, fixture string
		resume, fork  bool
		effort        string
		want          []any
	}{
		{"a fork of a session at high", "fork-held-effort", false, true, "", []any{defaultEffort}},
		{"a resume already at the default", "resume-default-effort", true, false, "", nil},
		{"a new session at OpenCode's own pick", "new-effort", false, false, "", nil},
		{"a resume with an effort of its own", "resume-held-effort", true, false, "low", []any{"low"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &play{fixture: fixture(t, tc.fixture)}
			spec := p.spec(t)
			spec.Model, spec.Effort = effortModel, tc.effort
			if tc.resume {
				spec.NativeSessionID = recordedSession(t, p.fixture, "session/resume")
			}
			if tc.fork {
				spec.ForkFrom = recordedSession(t, p.fixture, "session/fork")
			}
			_, o := run(t, spec, nil)
			if o.State != v1.RunSucceeded {
				t.Fatalf("outcome %s %+v", o.State, o.Error)
			}
			got := efforts(p.seen(t))
			if len(got) != len(tc.want) {
				t.Fatalf("effort set %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("effort set %v, want %v", got, tc.want)
				}
			}
		})
	}
}
