package claude

import (
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
)

// The outcome rule as a table: every kind of result, final or not, with and
// without an interrupt, a cancel, or a steer Claude took after it. Rounds of
// review found this rule broken three times, each time along a path the fix
// before had not enumerated; the table is the enumeration.
func TestOutcomeRule(t *testing.T) {
	const session = "6f1c2b1e-9d3a-4c55-8e7f-0a1b2c3d4e5f"
	results := map[string]string{
		"success":         `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"` + session + `"}`,
		"error":           `{"type":"result","subtype":"success","is_error":true,"result":"bad model","terminal_reason":"api_error","session_id":"` + session + `"}`,
		"prompt too long": `{"type":"result","subtype":"success","is_error":true,"result":"Prompt is too long","terminal_reason":"prompt_too_long","session_id":"` + session + `"}`,
		"usage limit":     `{"type":"result","subtype":"success","is_error":true,"result":"You've hit your limit","api_error_status":429,"session_id":"` + session + `"}`,
		"interrupted":     `{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_streaming","session_id":"` + session + `"}`,
		"not found":       `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["No conversation found with session ID: x"],"session_id":"` + session + `"}`,
	}
	const replay = `{"type":"user","isReplay":true,"message":{"role":"user","content":"steer"}}`
	const queued = `{"type":"result","subtype":"success","is_error":false,"result":"done","queued_turn_count":1,"session_id":"` + session + `"}`

	cases := []struct {
		name   string
		lines  []string
		e      ended
		state  v1.RunState
		class  string
		answer bool // FinalText carries the result
	}{
		// No result at all.
		{name: "no result", e: ended{}, state: v1.RunFailed, class: adapter.ClassHarnessExited},
		{name: "no result, interrupted", e: ended{interrupted: true}, state: v1.RunCancelled},
		{name: "no result, cancelled", e: ended{cancelled: true}, state: v1.RunCancelled},

		// A final result says what happened.
		{name: "final success", lines: []string{results["success"]}, e: ended{final: true}, state: v1.RunSucceeded, answer: true},
		{name: "final success, interrupt too late", lines: []string{results["success"]}, e: ended{final: true, interrupted: true}, state: v1.RunSucceeded, answer: true},
		{name: "final success, cancelled after", lines: []string{results["success"]}, e: ended{final: true, cancelled: true}, state: v1.RunSucceeded, answer: true},
		{name: "final error", lines: []string{results["error"]}, e: ended{final: true}, state: v1.RunFailed, class: adapter.ClassHarness},
		{name: "final prompt too long", lines: []string{results["prompt too long"]}, e: ended{final: true}, state: v1.RunFailed, class: adapter.ClassPromptTooLong},
		{name: "final usage limit", lines: []string{results["usage limit"]}, e: ended{final: true}, state: v1.RunFailed, class: adapter.ClassUsageLimit},
		{name: "final after interrupt", lines: []string{results["interrupted"]}, e: ended{final: true, interrupted: true}, state: v1.RunCancelled},
		{name: "final error, cancelled after", lines: []string{results["error"]}, e: ended{final: true, cancelled: true}, state: v1.RunFailed, class: adapter.ClassHarness},

		// An error before Claude read any input is not final by the count,
		// and is still the answer: nothing else will come.
		{name: "not found before reading input", lines: []string{results["not found"]}, e: ended{}, state: v1.RunFailed, class: adapter.ClassSessionNotFound},

		// A result that was not final decides nothing by what it says.
		{name: "success, steer taken after, died", lines: []string{results["success"], replay}, e: ended{}, state: v1.RunFailed, class: adapter.ClassHarnessExited},
		{name: "success, steer queued, died", lines: []string{queued}, e: ended{}, state: v1.RunFailed, class: adapter.ClassHarnessExited},
		{name: "success, steer never read, died", lines: []string{results["success"]}, e: ended{}, state: v1.RunFailed, class: adapter.ClassHarnessExited},
		{name: "success, steer taken after, interrupted", lines: []string{results["success"], replay}, e: ended{interrupted: true}, state: v1.RunCancelled},
		{name: "success, steer taken after, cancelled", lines: []string{results["success"], replay}, e: ended{cancelled: true}, state: v1.RunCancelled},
		{name: "error, steer taken after, died", lines: []string{results["error"], replay}, e: ended{}, state: v1.RunFailed, class: adapter.ClassHarnessExited},
		{name: "prompt too long, steer taken after, died", lines: []string{results["prompt too long"], replay}, e: ended{}, state: v1.RunFailed, class: adapter.ClassHarnessExited},
		{name: "usage limit, steer taken after, cancelled", lines: []string{results["usage limit"], replay}, e: ended{cancelled: true}, state: v1.RunCancelled},
		{name: "error, steer taken after, interrupted", lines: []string{results["error"], replay}, e: ended{interrupted: true}, state: v1.RunCancelled},
		{name: "error, not final, cancelled", lines: []string{results["error"]}, e: ended{cancelled: true}, state: v1.RunCancelled},

		// A steer taken and answered: the later result is final and decides.
		{name: "steer answered", lines: []string{results["success"], replay, results["error"]}, e: ended{final: true}, state: v1.RunFailed, class: adapter.ClassHarness},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := newTranslator(session, func(v1.Event) {})
			for _, l := range c.lines {
				tr.line([]byte(l))
			}
			out := tr.outcome(c.e)
			if out.State != c.state {
				t.Fatalf("state %s, want %s (%+v)", out.State, c.state, out.Error)
			}
			switch {
			case c.class == "" && out.Error != nil:
				t.Errorf("error %+v, want none", out.Error)
			case c.class != "" && (out.Error == nil || out.Error.Class != c.class):
				t.Errorf("error %+v, want class %s", out.Error, c.class)
			}
			if (out.FinalText != "") != c.answer {
				t.Errorf("final text %q; want an answer: %v", out.FinalText, c.answer)
			}
			if (out.Limit != nil) != (c.class == adapter.ClassUsageLimit) {
				t.Errorf("limit %+v with class %s", out.Limit, c.class)
			}
		})
	}
}

// A limit Claude got past is not carried into a later, unrelated error.
func TestRecoveredLimitIsForgotten(t *testing.T) {
	const session = "6f1c2b1e-9d3a-4c55-8e7f-0a1b2c3d4e5f"
	rejected := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1789786800,"rateLimitType":"five_hour"}}`
	allowed := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1789786800,"rateLimitType":"five_hour"}}`
	success := `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"` + session + `"}`
	replay := `{"type":"user","isReplay":true,"message":{"role":"user","content":"steer"}}`
	failure := `{"type":"result","subtype":"success","is_error":true,"result":"bad","session_id":"` + session + `"}`
	cases := map[string][]string{
		"a success in between":   {rejected, success, replay, failure},
		"an allowed event after": {rejected, allowed, failure},
		"still rejected is kept": {rejected, failure},
	}
	for name, lines := range cases {
		t.Run(name, func(t *testing.T) {
			tr := newTranslator(session, func(v1.Event) {})
			for _, l := range lines {
				tr.line([]byte(l))
			}
			out := tr.outcome(ended{final: true})
			keep := name == "still rejected is kept"
			if keep != (out.Error != nil && out.Error.Class == adapter.ClassUsageLimit) {
				t.Errorf("outcome %+v", out.Error)
			}
			if keep && (out.Limit == nil || !out.Limit.ResetAt.Equal(time.Unix(1789786800, 0))) {
				t.Errorf("limit %+v", out.Limit)
			}
		})
	}
}
