// Package claude drives Claude Code: `claude -p` with stream-json in both
// directions, a YAD-chosen --session-id, stdin held open for the control
// protocol, and --include-partial-messages so the inactivity watchdog can tell
// a thinking turn from a wedged one (decision 0006, ARCHITECTURE.md §3).
//
// Built in epic E2 (Zumino yad/dev). Until then Start refuses.
package claude

import (
	"context"
	"errors"

	"github.com/skkap/yad/internal/adapter"
)

// Adapter is the Claude Code adapter.
type Adapter struct{}

func (Adapter) Harness() string { return "claude" }

func (Adapter) Start(context.Context, adapter.Spec) (adapter.Turn, error) {
	return nil, errors.New("the claude adapter arrives in epic E2 (Zumino yad/dev) — see ARCHITECTURE.md §9")
}
