// Package agents knows which coding-agent CLIs exist, how to find them on this
// machine, and how to ask each one its version.
//
// This is the whole of YAD's capability story: a runner is useful to a control
// plane exactly insofar as it can say "I have claude 2.4.1 and codex 0.58 on
// this box, and here is what they can be pointed at".
package agents

import "runtime"

// Kind separates the two agents YAD drives properly from the ones it merely
// notices. A recognised agent is reported to the control plane so a human can
// see it; only a first-class one may be the target of a run.
type Kind string

const (
	// FirstClass — YAD knows this CLI's streaming format, its session-resume
	// flag and how to cancel it mid-turn.
	FirstClass Kind = "first-class"
	// Recognised — detected and reported, but no adapter yet. Listed so the
	// gap is visible rather than silent.
	Recognised Kind = "recognised"
)

// Agent is a coding-agent CLI YAD knows by name.
// The fields a control plane has no business seeing — how we find the binary
// and how we ask it its version — are `json:"-"`. The capability document is a
// public surface, and every field in it is a field someone will come to depend
// on.
type Agent struct {
	ID          string   `json:"id"`               // stable identifier used in the protocol and on the CLI
	Label       string   `json:"label"`            // what to print to a human
	Kind        Kind     `json:"kind"`             // whether YAD can actually drive it
	Models      []string `json:"models,omitempty"` // model ids that may be requested; empty = server decides
	Binary      string   `json:"-"`                // what to look for on PATH
	VersionArgs []string `json:"-"`                // arguments that make it print its version and exit
	EnvPath     string   `json:"-"`                // env var that overrides the binary path
}

// Catalog is every agent YAD will look for. Order is display order.
//
// Adding an entry here makes the agent *visible*; making it FirstClass means
// writing an adapter under internal/adapters, and nothing else may promote it.
func Catalog() []Agent {
	return []Agent{
		{
			ID:          "claude",
			Binary:      "claude",
			Label:       "Claude Code",
			Kind:        FirstClass,
			VersionArgs: []string{"--version"},
			EnvPath:     "YAD_CLAUDE_PATH",
			Models:      []string{"opus", "sonnet", "haiku"},
		},
		{
			ID:          "codex",
			Binary:      "codex",
			Label:       "Codex",
			Kind:        FirstClass,
			VersionArgs: []string{"--version"},
			EnvPath:     "YAD_CODEX_PATH",
		},
		{ID: "gemini", Binary: "gemini", Label: "Gemini CLI", Kind: Recognised, VersionArgs: []string{"--version"}, EnvPath: "YAD_GEMINI_PATH"},
		{ID: "copilot", Binary: "copilot", Label: "GitHub Copilot CLI", Kind: Recognised, VersionArgs: []string{"--version"}, EnvPath: "YAD_COPILOT_PATH"},
		{ID: "opencode", Binary: "opencode", Label: "OpenCode", Kind: Recognised, VersionArgs: []string{"--version"}, EnvPath: "YAD_OPENCODE_PATH"},
		{ID: "cursor", Binary: "cursor-agent", Label: "Cursor Agent", Kind: Recognised, VersionArgs: []string{"--version"}, EnvPath: "YAD_CURSOR_PATH"},
	}
}

// Platform is the os/arch pair reported alongside the agent list, so a control
// plane can route a run to a machine that can actually build the thing.
func Platform() (goos, goarch string) {
	return runtime.GOOS, runtime.GOARCH
}
