// Package harness knows which coding-agent CLIs exist, how to find them on this
// machine, and how to ask each one its version.
//
// A runner is useful to a hub exactly insofar as it can say "I have claude 2.1
// and codex 0.147 on this box" — so detection is the first half of the
// capability document, and it never fails: absence is reported, not raised.
package harness

import "runtime"

// Kind separates the harnesses YAD drives from the ones it merely notices. A
// recognised harness is reported so a human can see the gap; only a first-class
// one may be the target of a run.
type Kind string

const (
	// FirstClass — an adapter exists under internal/adapter that knows the
	// streaming format, how to resume a session and how to interrupt a turn.
	FirstClass Kind = "first-class"
	// Recognised — detected and reported, refused as a target.
	Recognised Kind = "recognised"
)

// Harness is a coding-agent CLI YAD knows by name.
//
// The fields a hub has no business seeing — how the binary is found and how
// its version is asked for — are `json:"-"`. The capability document is a
// public surface, and every field in it is one somebody will depend on.
type Harness struct {
	ID          string   `json:"id"`               // stable; used in the protocol and on the CLI
	Label       string   `json:"label"`            // for humans
	Kind        Kind     `json:"kind"`             // whether YAD can actually drive it
	Models      []string `json:"models,omitempty"` // aliases a run may request; empty = the harness decides
	Binary      string   `json:"-"`
	VersionArgs []string `json:"-"`
	EnvPath     string   `json:"-"` // env var overriding the binary path, for GUI-launched daemons with no shell PATH
}

// Catalog is every harness YAD looks for, in display order.
//
// Adding an entry makes a harness *visible*. Making it FirstClass means writing
// its adapter, and nothing else may promote it — a runner must never accept a
// run it cannot drive.
func Catalog() []Harness {
	return []Harness{
		// Claude and Codex are the two harnesses YAD exists to drive, and each
		// has its adapter (internal/adapter/claude, internal/adapter/codex).
		// Codex names no models: which it offers depends on the account's plan,
		// so a run's model is passed through and Codex decides.
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
		// OpenCode is driven over the Agent Client Protocol
		// (internal/adapter/opencode on internal/adapter/acp, decision 0070).
		// Its models depend on the providers it is logged in to, so, as for
		// Codex, a run's model is passed through and OpenCode decides.
		{ID: "opencode", Binary: "opencode", Label: "OpenCode", Kind: FirstClass, VersionArgs: []string{"--version"}, EnvPath: "YAD_OPENCODE_PATH"},
		{ID: "cursor", Binary: "cursor-agent", Label: "Cursor Agent", Kind: Recognised, VersionArgs: []string{"--version"}, EnvPath: "YAD_CURSOR_PATH"},
	}
}

// Lookup returns the catalog entry with this id.
func Lookup(id string) (Harness, bool) {
	for _, h := range Catalog() {
		if h.ID == id {
			return h, true
		}
	}
	return Harness{}, false
}

// Platform is the os/arch pair reported beside the harness list, so a hub can
// route a run to a machine that can actually build the thing.
func Platform() (goos, goarch string) {
	return runtime.GOOS, runtime.GOARCH
}
