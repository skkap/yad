package v1

import "time"

// Capabilities is the capability document: what a runner advertises. Every
// field is derived from the machine or the owner's config, so two runners with
// the same document are interchangeable for routing.
type Capabilities struct {
	RunnerID         string          `json:"runner_id" doc:"The runner's stable id: random, written once to its profile, and the same at every hub it registers with. It is the {runner} in the sync and deregister paths. Not a secret."`
	Name             string          `json:"name" doc:"A name for people: the owner's choice, or the machine's hostname. For display only; two runners may share one."`
	YadVersion       string          `json:"yad_version" doc:"The yad build: v0.4.0, v0.4.0-4-gabc1234 for a build four commits after it, or dev for an unstamped one. What a hub's min_version is compared against."`
	OS               string          `json:"os" doc:"The operating system as Go names it: linux or darwin."`
	Arch             string          `json:"arch" doc:"The CPU architecture as Go names it: amd64 or arm64."`
	Labels           []string        `json:"labels,omitempty" doc:"Free-form routing strings the owner attached, such as linux, gpu or work. The runner never interprets them; a hub may route on them."`
	Harnesses        []HarnessReport `json:"harnesses" doc:"Every harness in the runner's catalog, installed or not. A run may be offered only for a harness whose kind is first-class, present is true and error is empty."`
	HostTools        []HostTool      `json:"host_tools,omitempty" doc:"The non-harness tools a run may need, as they exist on this runner. A tool is usable when present is true and error is empty, and, for a tool that has a login, when logged_in is true as well. A tool that is missing or broken is reported rather than left out."`
	Capacity         Capacity        `json:"capacity" doc:"How many runs the runner executes at once, and the owner's per-harness caps. The configured size, not what is free now: that is each sync's health.free_capacity."`
	ProtocolFeatures []string        `json:"protocol_features,omitempty" doc:"The protocol features beyond the v1 baseline this runner acts on: start_at, steer, interrupt, drain, close_session, effort, login and accounts; live_sessions is reserved. accounts is per hub: a runner lists it only to a hub its owner lets add and remove accounts, and only beside login, so two hubs of one runner may be sent different lists and fingerprints. A hub uses none that is not listed here, because nothing acknowledges a control and an ignored one looks exactly like an obeyed one. Ignore strings you do not know."`
	// PathSources is sent only as false, so the document of every runner
	// that takes them — and of every runner older than the field — is the
	// same, fingerprint included (decision 0062).
	PathSources *bool     `json:"path_sources,omitempty" doc:"false: this runner's owner has switched sources on the machine off, so a run with a path source, or a git source whose url is a path or file:// URL, fails with source_refused. Offer a run opening a session (session.new) with one to another runner; a run in a session already bound to this runner can go nowhere else, so offer it, and the refusal names the setting. Absent: the runner takes them inside the directories its owner allows. Never sent as true."`
	ObservedAt  time.Time `json:"observed_at" doc:"When the runner built this document. Left out of the fingerprint, so it changes without the fingerprint moving."`
}

// HarnessReport is one harness as it exists on the runner. Accounts appear by
// label and state only; credentials never leave the machine.
type HarnessReport struct {
	ID    string `json:"id" doc:"The id a run names in its harness field: claude, codex, gemini, copilot, opencode or cursor."`
	Label string `json:"label" doc:"The harness's display name, such as Claude Code."`
	Kind  string `json:"kind" enum:"first-class,recognised" doc:"first-class: the runner has an adapter for it and can run it. recognised: detected and reported so the gap is visible, and never the target of a run."`
	// Present is there being a binary the runner starts for this harness —
	// the one a YAD_<ID>_PATH override names, or PATH's when the override
	// names nothing (DEV-68). It says nothing about whether it works; Error
	// does.
	Present bool `json:"present" doc:"There is a binary the runner starts for this harness's runs. Not that it works: a present harness with an error cannot take runs. A harness absent with an error has a path configured for it that names nothing and none on the runner's PATH either."`
	// Version is the version number and nothing else of what the harness
	// printed: the line around it can hold a path or a credential (DEV-67).
	Version string `json:"version,omitempty" doc:"The version number the harness reported, such as 2.4.1 or 1.0.0-beta.12, and nothing else of what it printed. Absent when it printed no version."`
	// Error is the runner's own words, never the harness's: a child's output
	// and the path it was started from stay on the machine (DEV-60).
	Error string `json:"error,omitempty" doc:"Why this harness cannot take runs, and the next action for whoever owns the machine. Written by the runner: it never quotes what the harness printed and never names a path on the machine."`
	// Models are asked of the harness itself, for each login a run may use —
	// Claude's list_models control request, Codex's model/list — without a
	// turn, so without a token (DEV-50). Where it could not be asked, the
	// runner's catalog answers, and ModelsSource says so.
	Models []string `json:"models,omitempty" doc:"Models the harness offers on this runner, such as opus or gpt-5.5, in the harness's own order: every model offered to any login a run may use, once. Where the logins differ, each account's models say what its own is offered. Not a limit: a run may name any model, and the harness decides whether it exists. Absent when the runner knows none."`
	// ModelsSource is set whenever Models is.
	ModelsSource string          `json:"models_source,omitempty" enum:"harness,catalog" doc:"Where models came from. harness: the harness listed them, asked without spending a token for the logins a run may use. catalog: the harness could not be asked or did not answer, and models is the runner's own fixed list for it, which may name a model the login is not offered or miss one it is. Absent when models is, and from runners older than this field."`
	Accounts     []AccountReport `json:"accounts,omitempty" doc:"The owner's accounts for this harness. Absent when none are configured, and the harness runs on its own login."`
	// Warnings are what the runner found wrong with a harness it can still
	// drive — an installed Codex whose app-server protocol differs from the
	// one the adapter was built against, a path override naming nothing while
	// PATH has the harness. A hub may show them or prefer a runner without;
	// they never make a harness refuse runs, which Error does. The same rule
	// as Error binds them (DEV-67).
	Warnings []string `json:"warnings,omitempty" doc:"What is wrong with a harness the runner can still drive, each with the next action for whoever owns the machine: a path configured for it that names nothing, so the one on PATH is used; a Codex whose protocol differs from the one the runner was built against. Never a reason to refuse runs. Written by the runner: it never quotes what the harness printed and never names a path on the machine."`
}

// Where a harness's models came from: HarnessReport.ModelsSource.
const (
	// ModelsFromHarness is a list the harness gave for the logins runs use.
	ModelsFromHarness = "harness"
	// ModelsFromCatalog is the runner's own fixed list, reported because the
	// harness could not be asked or did not answer.
	ModelsFromCatalog = "catalog"
)

// AccountState is what a hub may know about an account, and the whole of it.
// The words are DOMAIN.md's: an account is free, limited until a reset, or
// needs login.
type AccountState string

const (
	// AccountFree takes runs.
	AccountFree AccountState = "free"
	// AccountLimited is at a usage limit until LimitedUntil.
	AccountLimited AccountState = "limited"
	// AccountNeedsLogin cannot run a turn until the owner logs it in again:
	// its harness home holds no working login, or the home is not there at
	// all. The owner runs `yad account add` at the machine, or logs it in
	// from a hub advertising the login feature (decision 0055). Skipped for
	// claiming exactly as a limited account is, and never an error that stops
	// a runner registering.
	AccountNeedsLogin AccountState = "needs_login"
)

// AccountStates lists the closed set.
func AccountStates() []AccountState {
	return []AccountState{AccountFree, AccountLimited, AccountNeedsLogin}
}

// AccountReport is an account's public face: its label, its state, and until
// when a limit lasts. A label is not a secret and a credential is — nothing
// else from an account's home is reportable, and none of it appears here.
type AccountReport struct {
	Label string `json:"label" doc:"The owner's name for the account, such as work. Not a secret, and not the identity it is signed in as."`
	// omitempty keeps state out of the schema's required list. Every runner
	// that has this field always sets it — Report substitutes free for an
	// empty one — so the wire is unchanged; what it buys is that a runner
	// from before this field still validates against a hub generated after
	// it. Hubs update centrally and runners sit on other people's machines,
	// so that is the direction that matters, and §2's rule is that a field
	// added within v1 never breaks an older runner.
	State        AccountState `json:"state,omitempty" enum:"free,limited,needs_login" doc:"free takes runs. limited is at a usage limit until limited_until. needs_login cannot run a turn until the owner logs it in again, at the machine or with a hub login (start_login or login_token). Absent from runners older than this field: the runner cannot say, which is not a fault."`
	LimitedUntil *time.Time   `json:"limited_until,omitempty" doc:"When a limited account's usage limit resets. Absent for an account that is not limited."`
	// Windows is every usage window the harness last told the runner about,
	// whether or not the account is at a limit: a hub seeing one at 96% knows
	// why a runner will stop claiming soon, and one at 100% with a reset says
	// why it has stopped (decision 0039).
	//
	// Reported from every run, so a window is as fresh as the last turn that
	// ran on the account and no fresher. Absent means no run has yet heard a
	// window from this harness, never that the account has no limits.
	Windows []AccountWindow `json:"windows,omitempty" doc:"Every usage window the harness last reported for this account, whether or not it is at a limit, so a hub sees an account running low before it runs out. As fresh as the last turn that ran on it. Absent means no run has heard a window yet, never that the account has no limits."`
	// Models is per account because a plan decides them: two logins of one
	// harness can be offered different models (DEV-50).
	Models []string `json:"models,omitempty" doc:"The models the harness listed for this account's login, in its own order: those a run on this account may name. Absent when the harness has not listed them for it — the account needs login, or the harness did not answer — and the harness's models are then the runner's best answer."`
}

// AccountWindow is one usage window of one account, as its harness names it:
// Claude's `five_hour` and `seven_day`, Codex's `primary` and `secondary`.
//
// The names are each harness's own and are deliberately not unified. DOMAIN.md
// defines a usage limit as "Claude's five-hour and weekly limits, Codex's
// primary and secondary windows" — the vocabulary is harness-specific in the
// domain model, and a name invented here would be a third vocabulary that
// matches neither harness's own reporting.
type AccountWindow struct {
	Name string `json:"name" doc:"The window as its harness names it: five_hour or seven_day for Claude, primary or secondary for Codex."`
	// UsedPercent is 0-100. The unit is in the name because the harnesses
	// disagree: Codex reports a percentage and Claude a 0-1 fraction, and a
	// factor of a hundred is invisible in a number alone.
	UsedPercent float64 `json:"used_percent" doc:"How much of the window is used, 0-100."`
	// ResetsAt is absent when the harness reported the window's use without
	// saying when it refills.
	ResetsAt *time.Time `json:"resets_at,omitempty" doc:"When the window refills, if the harness said."`
}

// HostTool is a non-harness executable a run may need — git, gh, docker.
//
// A tool that is present is not necessarily usable: a gh nobody has signed in
// cannot open a pull request, and a docker whose daemon is down cannot run a
// container. A hub routing on a tool wants `present`, no `error`, and
// `logged_in` where the tool has a login.
type HostTool struct {
	ID string `json:"id" doc:"The tool: git, gh or docker."`
	// Present is the binary detection found and probed, under the same
	// override rule as a harness (DEV-68), and the one a run uses: yad's own
	// git resolves it the same way, and a harness's children find it ahead of
	// PATH (0045).
	Present bool   `json:"present" doc:"The runner found a binary for this tool and probed it: the one its configured path names, or the one on PATH when that path names nothing. Present is not usable: see error and logged_in."`
	Version string `json:"version,omitempty" doc:"The version number the tool reported, such as 2.51.0, and nothing else of what it printed. Absent when it printed no version."`
	// LoggedIn is nil when the tool has no notion of a login, and nil too when
	// it has one and the runner could not find out — Error says why.
	LoggedIn *bool `json:"logged_in,omitempty" doc:"Whether the tool is signed in. Absent for a tool with no login, and for one whose login state the runner could not find out, where error says why."`
	// LoginHosts are the hosts the tool is signed in to — github.com, a GitHub
	// Enterprise hostname, or both — so a hub can tell a runner that can reach
	// its repositories from one that cannot. Never who it is signed in as: an
	// account name is the machine owner's, not the hub's.
	LoginHosts []string `json:"login_hosts,omitempty" doc:"Every host the tool is signed in to, such as github.com or a GitHub Enterprise hostname; a gh signed in to two reports both. Never the account it is signed in as."`
	// Error is what is wrong with this tool on the runner: a path configured
	// for it that names nothing with none on PATH either, a probe that timed
	// out, a binary that would not run, a Docker daemon that is not answering.
	// It carries the next action, and it never stops a runner registering —
	// absence and breakage are both facts a hub routes around. It can
	// accompany present: false: a tool the owner configured a path for, which
	// is not there, is both absent and worth explaining.
	Error string `json:"error,omitempty" doc:"What is wrong with this tool on the runner: a path configured for it that names nothing with none on PATH either, a probe that timed out, a binary that would not run, a Docker daemon that is not answering. May accompany present: false, when a configured path names nothing and PATH has no such tool. Carries the next action. A runner with one still registers."`
	// Warnings are what is wrong with a tool that still works, under the same
	// rule as HarnessReport.Warnings. Added within v1 for DEV-68, so a hub
	// generated before it ignores the field.
	Warnings []string `json:"warnings,omitempty" doc:"What is wrong with a tool the runner can still use, each with the next action for whoever owns the machine: a path configured for it that names nothing, so the one on PATH is used. Never a reason to treat the tool as unusable. Written by the runner: it never quotes what the tool printed and never names a path on the machine."`
}

// Capacity is how many runs a runner executes at once: one pool, with the
// owner's optional per-harness caps. Per-connection caps are the runner's
// business and are not advertised.
type Capacity struct {
	Total     int            `json:"total" doc:"Runs in all, across every harness."`
	ByHarness map[string]int `json:"by_harness,omitempty" doc:"Per-harness bounds by harness id, each applying independently of total. A harness missing from the map is bounded by total alone: the map carries only the caps the owner set, and is absent when there are none."`
}

// RegisterRequest is sent once, with the registration token as the bearer.
type RegisterRequest struct {
	Capabilities Capabilities `json:"capabilities" doc:"The runner's capability document. Keep it: it says what the runner can take until a sync carries a newer one."`
}

// RegisterResponse carries the runner credential — the only secret a runner
// keeps — and the hub's own features and timings.
type RegisterResponse struct {
	RunnerCredential string   `json:"runner_credential" doc:"The secret the runner sends as its bearer on every later call. Issued here and nowhere else; keep only what recognises it, such as a hash."`
	HubFeatures      []string `json:"hub_features,omitempty" doc:"Features this hub has beyond the v1 baseline. v1 defines none, and a hub that sends none is complete. A value v1 adds to one of its enums later is sent only to a hub advertising the feature that adds it."`
	SyncIntervalMS   int      `json:"sync_interval_ms" doc:"Milliseconds until the runner's next sync, 5000 to 60000 inclusive. A runner clamps a value outside that range and adds its own jitter."`
	LeaseMS          int      `json:"lease_ms" doc:"The lease the hub will name in its sync answers: how long a run offered to this runner, or held by it, is kept for it without a sync. Never shorter than sync_interval_ms."`
	MinVersion       string   `json:"min_version,omitempty" doc:"The oldest yad this hub takes, such as 0.4.0, compared on the release core alone. Absent is no floor."`
}

// DeregisterRequest retires a runner's credential; runs it still holds become
// lost on the hub's side.
type DeregisterRequest struct {
	Reason string `json:"reason,omitempty" doc:"Why the runner is leaving, in its own words, for the hub to show beside the runs it settles. Untrusted text: store it bounded and printable."`
}

// Ack is the empty success body, so every response is a JSON object a hub can
// extend later without breaking decoders.
type Ack struct {
	OK bool `json:"ok" doc:"Always true. The object exists so a hub can add fields later."`
}
