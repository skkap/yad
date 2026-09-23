package claude

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// Every tool result says whether it failed, from Claude's own is_error and
// never from the output (DEV-125). Recorded: a command that exited 3, one that
// succeeded and a Read of no file (2.1.280); two calls the permission mode
// refused (2.1.276, permission-denied.jsonl lines 25 and 71); a Read that
// succeeded, whose result carries no is_error at all, which the Messages API
// defines as false. Claude reports no exit status as a number, so none has
// an exit_code, and the output is exactly what it was.
func TestToolResultsSayWhetherTheyFailed(t *testing.T) {
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	for _, tc := range []struct {
		name, fixture string
		want          []bool
		outputs       []string
	}{
		{"a failing and a succeeding command, a missing file", abs("testdata/claude-2.1.280/tool-outcomes.jsonl"),
			[]bool{true, false, true}, []string{"Exit code 3\nout", "ok"}},
		{"refused by the permission mode", fixture("permission-denied"), []bool{true, true}, nil},
		{"a Read with no is_error", fixture("tool"), []bool{false}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &harness{fixture: tc.fixture}
			evs, _, _ := drive(t, context.Background(), h.spec(t), nil)
			results := kinds(evs, v1.EventToolResult)
			if len(results) != len(tc.want) {
				t.Fatalf("%d tool results, want %d: %v", len(results), len(tc.want), results)
			}
			for i, r := range results {
				if r.Tool.IsError == nil || *r.Tool.IsError != tc.want[i] {
					t.Errorf("result %d (%q): is_error %s, want %v", i, r.Tool.Output, show(r.Tool.IsError), tc.want[i])
				}
				if r.Tool.ExitCode != nil {
					t.Errorf("result %d: exit_code %d, which Claude never reports as a number", i, *r.Tool.ExitCode)
				}
				if i < len(tc.outputs) && r.Tool.Output != tc.outputs[i] {
					t.Errorf("result %d output = %q, want %q unchanged", i, r.Tool.Output, tc.outputs[i])
				}
			}
		})
	}
}

func show(b *bool) string {
	if b == nil {
		return "absent"
	}
	return fmt.Sprint(*b)
}
