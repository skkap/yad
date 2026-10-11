package acp

import (
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
)

// An agent that declares no default effort has nothing to set a resumed
// session back to: a run without an effort is prompted at whatever the
// session opens with, and the core sends no option the agent never named.
func TestAnAgentWithNoDefaultEffortIsPromptedAsTheSessionOpens(t *testing.T) {
	_, o := play(t, "resume-effort", func(s *adapter.Spec) { s.NativeSessionID = "s0" })
	if o.State != v1.RunSucceeded {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}
	if got := sent(t, "session/set_config_option"); len(got) != 0 {
		t.Errorf("options set %v", got)
	}
}
