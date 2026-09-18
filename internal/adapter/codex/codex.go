// Package codex drives Codex through `codex app-server` over stdio JSON-RPC:
// thread/start or thread/resume, turn/start, turn/steer, turn/interrupt, checked
// against the schema the installed version generates (decision 0006,
// ARCHITECTURE.md §3).
//
// Built in epic E5 (Zumino yad/dev). Until then Start refuses.
package codex

import (
	"context"
	"errors"

	"github.com/skkap/yad/internal/adapter"
)

// Adapter is the Codex adapter.
type Adapter struct{}

func (Adapter) Harness() string { return "codex" }

func (Adapter) Start(context.Context, adapter.Spec) (adapter.Turn, error) {
	return nil, errors.New("the codex adapter arrives in epic E5 (Zumino yad/dev) — see ARCHITECTURE.md §9")
}
