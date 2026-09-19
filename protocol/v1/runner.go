package v1

import "time"

// Capabilities is the capability document: what a runner advertises. Every
// field is derived from the machine or the owner's config, so two runners with
// the same document are interchangeable for routing.
type Capabilities struct {
	RunnerID         string          `json:"runner_id"`
	Name             string          `json:"name"`
	YadVersion       string          `json:"yad_version"`
	OS               string          `json:"os"`
	Arch             string          `json:"arch"`
	Labels           []string        `json:"labels,omitempty"`
	Harnesses        []HarnessReport `json:"harnesses"`
	HostTools        []HostTool      `json:"host_tools,omitempty"`
	Capacity         Capacity        `json:"capacity"`
	ProtocolFeatures []string        `json:"protocol_features,omitempty"`
	ObservedAt       time.Time       `json:"observed_at"`
}

// HarnessReport is one harness as it exists on the runner. Accounts appear by
// label only; credentials never leave the machine.
type HarnessReport struct {
	ID       string          `json:"id"`
	Label    string          `json:"label"`
	Kind     string          `json:"kind" enum:"first-class,recognised"`
	Present  bool            `json:"present"`
	Version  string          `json:"version,omitempty"`
	Error    string          `json:"error,omitempty"`
	Models   []string        `json:"models,omitempty"`
	Accounts []AccountReport `json:"accounts,omitempty"`
	// Warnings are what the runner found wrong with a harness it can still
	// drive — an installed Codex whose app-server protocol differs from the
	// one the adapter was built against. A hub may show them or prefer a
	// runner without; they never make a harness refuse runs, which Error does.
	Warnings []string `json:"warnings,omitempty"`
}

// AccountReport is an account's public face: its label and whether it is at a
// usage limit, and until when.
type AccountReport struct {
	Label        string     `json:"label"`
	LimitedUntil *time.Time `json:"limited_until,omitempty"`
}

// HostTool is a non-harness executable a run may need — git, gh, docker.
//
// A tool that is present is not necessarily usable: a gh nobody has signed in
// cannot open a pull request, and a docker whose daemon is down cannot run a
// container. A hub routing on a tool wants `present`, no `error`, and
// `logged_in` where the tool has a login.
type HostTool struct {
	ID      string `json:"id"`
	Present bool   `json:"present"`
	Version string `json:"version,omitempty"`
	// LoggedIn is nil when the tool has no notion of a login, and nil too when
	// it has one and the runner could not find out — Error says why.
	LoggedIn *bool `json:"logged_in,omitempty"`
	// LoginHost is the host the tool is signed in to — github.com, or a GitHub
	// Enterprise hostname — so a hub can tell a runner that can reach its
	// repositories from one that cannot. Never who it is signed in as: an
	// account name is the machine owner's, not the hub's.
	LoginHost string `json:"login_host,omitempty"`
	// Error is what is wrong with a tool that is installed: a probe that timed
	// out, a binary that would not run, a Docker daemon that is not answering.
	// It carries the next action, and it never stops a runner registering —
	// absence and breakage are both facts a hub routes around.
	Error string `json:"error,omitempty"`
}

// Capacity is how many runs a runner executes at once: one pool, with the
// owner's optional per-harness caps. Per-connection caps are the runner's
// business and are not advertised.
type Capacity struct {
	Total     int            `json:"total"`
	ByHarness map[string]int `json:"by_harness,omitempty"`
}

// RegisterRequest is sent once, with the registration token as the bearer.
type RegisterRequest struct {
	Capabilities Capabilities `json:"capabilities"`
}

// RegisterResponse carries the runner credential — the only secret a runner
// keeps — and the hub's own features and timings.
type RegisterResponse struct {
	RunnerCredential string   `json:"runner_credential"`
	HubFeatures      []string `json:"hub_features,omitempty"`
	SyncIntervalMS   int      `json:"sync_interval_ms"`
	LeaseMS          int      `json:"lease_ms"`
	MinVersion       string   `json:"min_version,omitempty"`
}

// DeregisterRequest retires a runner's credential; runs it still holds become
// lost on the hub's side.
type DeregisterRequest struct {
	Reason string `json:"reason,omitempty"`
}

// Ack is the empty success body, so every response is a JSON object a hub can
// extend later without breaking decoders.
type Ack struct {
	OK bool `json:"ok"`
}
