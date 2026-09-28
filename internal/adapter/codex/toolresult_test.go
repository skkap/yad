package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// outcome is a tool result's is_error and exit_code as text, "-" for absent,
// so a table can say all three answers at once.
func outcome(tool *v1.ToolEvent) string {
	failed, exit := "-", "-"
	if tool.IsError != nil {
		failed = fmt.Sprint(*tool.IsError)
	}
	if tool.ExitCode != nil {
		exit = fmt.Sprint(*tool.ExitCode)
	}
	return failed + " " + exit
}

// Every tool result says whether it failed, from what Codex reported and never
// from the output, and a command's carries its exit status too. Recorded from
// Codex 0.147.0: a command that exited 3, one that exited 0, a file created
// with apply_patch, and a command the owner's approval policy declined. The
// output keeps its "[exit N]" and "[declined: …]" tails, so a hub reading
// only the output loses nothing (DEV-125).
func TestToolResultsSayWhetherTheyFailed(t *testing.T) {
	for _, tc := range []struct {
		fixture  string
		settings map[string]string
		want     []string // is_error and exit_code per result, in order
		outputs  []string
	}{
		{fixture: "tool-outcomes", want: []string{"true 3", "false 0"}, outputs: []string{"out\n[exit 3]", "ok\n"}},
		{fixture: "file-change", want: []string{"false -"}},
		{fixture: "approval", settings: map[string]string{"approval": "untrusted", "sandbox": "read-only"},
			want: []string{"true -"}, outputs: []string{"[declined: the owner's approval policy did not allow it]"}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			h := &harness{fixture: fixture(tc.fixture)}
			spec := h.spec(t)
			if tc.settings != nil {
				spec.Settings = tc.settings
			}
			evs, out, _ := drive(t, context.Background(), spec, nil)
			if out.State != v1.RunSucceeded {
				t.Fatalf("outcome = %+v", out)
			}
			results := kinds(evs, v1.EventToolResult)
			if len(results) != len(tc.want) {
				t.Fatalf("%d tool results, want %d: %v", len(results), len(tc.want), results)
			}
			for i, r := range results {
				if got := outcome(r.Tool); got != tc.want[i] {
					t.Errorf("result %d (%q): is_error exit_code = %s, want %s", i, r.Tool.Output, got, tc.want[i])
				}
				if i < len(tc.outputs) && r.Tool.Output != tc.outputs[i] {
					t.Errorf("result %d output = %q, want %q unchanged", i, r.Tool.Output, tc.outputs[i])
				}
			}
		})
	}
}

// The kinds no recording reaches, from the fields the pinned protocol gives
// each: a status, an MCP error, a dynamic tool's success; and none at all for
// a web search, which is left absent rather than guessed.
func TestToolOutcomeByKind(t *testing.T) {
	for _, tc := range []struct {
		item, want string
	}{
		{`{"type":"commandExecution","status":"completed","exitCode":0}`, "false 0"},
		{`{"type":"commandExecution","status":"failed","exitCode":127}`, "true 127"},
		{`{"type":"commandExecution","status":"completed","exitCode":1}`, "true 1"},
		{`{"type":"commandExecution","status":"declined","exitCode":null}`, "true -"},
		{`{"type":"commandExecution","status":"failed"}`, "true -"},
		{`{"type":"fileChange","status":"failed"}`, "true -"},
		{`{"type":"fileChange","status":"declined"}`, "true -"},
		{`{"type":"fileChange","status":"inProgress"}`, "- -"},
		{`{"type":"mcpToolCall","status":"completed","result":{"content":[]}}`, "false -"},
		{`{"type":"mcpToolCall","status":"failed","error":{"message":"no such tool"}}`, "true -"},
		{`{"type":"mcpToolCall","status":"completed","error":{"message":"odd"}}`, "true -"},
		{`{"type":"dynamicToolCall","status":"completed","success":false}`, "true -"},
		{`{"type":"dynamicToolCall","status":"completed","success":true}`, "false -"},
		{`{"type":"dynamicToolCall","status":"failed","success":null}`, "true -"},
		{`{"type":"collabAgentToolCall","status":"completed"}`, "false -"},
		{`{"type":"collabAgentToolCall","status":"failed"}`, "true -"},
		{`{"type":"collabAgentToolCall","status":"interrupted"}`, "true -"},
		{`{"type":"webSearch","query":"yad"}`, "- -"},
		{`{"type":"imageView","path":"/x.png"}`, "- -"},
		{`{"type":"commandExecution","status":"someNewStatus"}`, "- -"},
	} {
		var it item
		if err := json.Unmarshal([]byte(tc.item), &it); err != nil {
			t.Fatal(err)
		}
		failed, exit := toolOutcome(it)
		if got := outcome(&v1.ToolEvent{IsError: failed, ExitCode: exit}); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.item, got, tc.want)
		}
	}
}
