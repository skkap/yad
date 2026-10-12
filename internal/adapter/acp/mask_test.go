package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/jsonrpc"
)

// What yad handed the agent, which its errors may quote back: a value in its
// environment and what a file grant holds, which is in no variable.
const (
	envToken     = "tok-env-0123456789abcdef"
	grantContent = "grant-file-content-42"
)

// said is the agent's error, quoting both; masked is how a run's error may
// quote it.
var (
	said   = "the login with " + envToken + " and " + grantContent + " was refused"
	masked = "the login with " + adapter.Redacted + " and " + adapter.Redacted + " was refused"
)

// Every refusal the core quotes into a run's error is masked of the run's
// environment and file grants, as its stderr is (DEV-192): the error goes to
// a hub, and an agent that could not use a credential may quote it back.
func TestAnAgentsRefusalIsMaskedInTheRunsError(t *testing.T) {
	ask := func(id int, method string) string {
		return fmt.Sprintf(`{">":{"jsonrpc":"2.0","id":%d,"method":%q,"params":{}}}`, id, method)
	}
	refuse := func(id int, msg string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32603,"message":"Internal error: %s"}}`, id, msg)
	}
	caps := func(c string) []string {
		return []string{ask(1, "initialize"), `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":` + c + `}}`}
	}
	opened := `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s1","configOptions":[{"id":"m","category":"model","currentValue":"a","options":[{"value":"a"},{"value":"b"}]}]}}`
	for _, tc := range []struct {
		name  string
		lines []string
		edit  func(*adapter.Spec)
		says  string
	}{
		{"a request", append(caps(`{}`), ask(2, "session/new"), refuse(2, said)),
			nil, "Agent refused session/new: "},
		{"the run's settings", append(caps(`{}`), ask(2, "session/new"), opened, ask(3, "session/set_config_option"), refuse(3, said)),
			func(s *adapter.Spec) { s.Model = "b" }, "Agent refused the run's settings: "},
		{"a resume, with no list to ask", append(caps(`{"sessionCapabilities":{"resume":{}}}`), ask(2, "session/resume"), refuse(2, said)),
			func(s *adapter.Spec) { s.NativeSessionID = "s0" }, "Agent refused session/resume: "},
		{"a resume of a session the agent lists", append(caps(`{"sessionCapabilities":{"resume":{},"list":{}}}`), ask(2, "session/resume"), refuse(2, said),
			ask(3, "session/list"), `{"jsonrpc":"2.0","id":3,"result":{"sessions":[{"sessionId":"s0","cwd":"/w"}]}}`),
			func(s *adapter.Spec) { s.NativeSessionID = "s0" }, "Agent would not resume session s0: "},
		{"a fork, and the list after it", append(caps(`{"sessionCapabilities":{"fork":{},"list":{}}}`), ask(2, "session/fork"), refuse(2, said),
			ask(3, "session/list"), refuse(3, "the list is down too")),
			func(s *adapter.Spec) { s.ForkFrom = "s0" }, "Agent refused to open the session: "},
		{"the prompt", append(caps(`{}`), ask(2, "session/new"), opened, ask(3, "session/prompt"), refuse(3, said)),
			nil, "Agent failed the turn: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conversation := filepath.Join(t.TempDir(), "conversation.jsonl")
			if err := os.WriteFile(conversation, []byte(strings.Join(tc.lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			events, o := play(t, conversation, func(s *adapter.Spec) {
				s.Env = append(s.Env, "YAD_TEST_TOKEN="+envToken)
				s.Secrets = []string{grantContent}
				if tc.edit != nil {
					tc.edit(s)
				}
			})
			if o.State != v1.RunFailed || o.Error == nil || o.Error.Class != adapter.ClassHarness {
				t.Fatalf("outcome %s %+v", o.State, o.Error)
			}
			if want := tc.says + "Internal error: " + masked; !strings.Contains(o.Error.Message, want) {
				t.Errorf("the run's error %q, want it to carry %q", o.Error.Message, want)
			}
			noSecret(t, o.Error.Message, events)
		})
	}
}

// What an agent's Classify quotes from the error's data, as OpenCode names
// the provider from there, is masked as its message is; so is a stop reason
// no ACP v1 turn ends with.
func TestWhatAnOutcomeQuotesOfTheAgentIsMasked(t *testing.T) {
	agent := testAgent
	agent.Classify = func(e *jsonrpc.RPCError, _ adapter.Spec) Failure {
		var d struct {
			ProviderID string `json:"providerID"`
		}
		json.Unmarshal(e.Data, &d)
		return Failure{Message: "the credential for " + d.ProviderID + " was refused: " + e.Message}
	}
	end := ended{env: []string{"YAD_TEST_TOKEN=" + envToken}, secrets: []string{grantContent}}
	for _, tc := range []struct {
		name string
		set  func(*translator)
		want string
	}{
		{"classified", func(tr *translator) {
			tr.agent = agent
			tr.promptErr = &jsonrpc.RPCError{Code: jsonrpc.CodeInternal, Message: said, Data: json.RawMessage(`{"providerID":"` + grantContent + `"}`)}
		}, "the credential for " + adapter.Redacted + " was refused: " + masked},
		{"a stop reason", func(tr *translator) {
			tr.done = &promptResult{StopReason: "stopped on " + envToken}
		}, `Agent ended the turn "stopped on ` + adapter.Redacted + `"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []v1.Event
			tr := newTranslator(func(e v1.Event) { events = append(events, e) }, testAgent, adapter.Spec{})
			tc.set(tr)
			o := tr.outcome(end)
			if o.Error == nil || !strings.Contains(o.Error.Message, tc.want) {
				t.Fatalf("outcome %s %+v, want the error to carry %q", o.State, o.Error, tc.want)
			}
			noSecret(t, o.Error.Message, events)
		})
	}
}

// noSecret fails the test if the run's error or any error event quotes what
// yad handed the agent.
func noSecret(t *testing.T, msg string, events []v1.Event) {
	t.Helper()
	all := []string{msg}
	for _, e := range events {
		if e.Error != nil {
			all = append(all, e.Error.Message)
		}
	}
	for _, s := range all {
		if strings.Contains(s, envToken) || strings.Contains(s, grantContent) {
			t.Errorf("a secret went out unmasked: %q", s)
		}
	}
}
