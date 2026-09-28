package v1

import "time"

// SyncRequest is the periodic call: heartbeat, lease renewal for every run
// listed, health report and the ask for work, in one request.
type SyncRequest struct {
	RunnerID    string `json:"runner_id" doc:"The runner's id: the same as the {runner} in the path, or the sync is refused."`
	Fingerprint string `json:"fingerprint" doc:"An opaque hash of the runner's capability document. Compare it with the one that came with the document you hold; when it differs and capabilities is absent, answer with a report_capabilities control."`
	// Capabilities is sent only when the hub asked for it with
	// ControlReportCapabilities, or on the first sync after a fingerprint move.
	Capabilities *Capabilities `json:"capabilities,omitempty" doc:"The full capability document, sent on the first sync of every runner process, after the fingerprint moves, and after a report_capabilities control. When present it replaces the one you hold. Its runner_id is the path's; a sync whose document names another runner is refused as invalid."`
	Health       Health        `json:"health" doc:"The runner's state for routing and alerting, computed fresh for this sync: free capacity, per-harness readiness, disk, load, what it owes this hub, recent errors, and whether it is draining."`
	// Runs lists every run the runner holds. Listing a run is how it is claimed
	// and how its lease is renewed; a run the hub offered and this list omits
	// was never received.
	Runs []HeldRun `json:"runs,omitempty" doc:"Every run the runner holds. Listing a run offered in the previous response claims it; listing a claimed run renews its lease. An offered run left out was never received, and goes back in the queue at this sync. A listed run the runner does not hold is answered with a cancel control for it. Each run is listed at most once; a hub handed two listings of one run may apply either."`
	// ClosedSessions are sessions this runner has closed and whose workdirs
	// it reclaims, each listed in every sync until one carrying it is
	// answered with a 2xx — at least once, so a hub records them by session
	// id and takes a repeat as the same news. A hub stops offering runs in a
	// session listed here: the runner refuses them. Sent by runners that
	// advertise the "close_session" feature, which also act on the
	// close_session control.
	ClosedSessions []ClosedSession `json:"closed_sessions,omitempty" doc:"Sessions this runner has closed, each repeated in every sync until one carrying it is answered with a 2xx. Record them by session id, take a repeat as the same news, stop offering runs in them, and end the runs still queued in them. A runner advertising close_session reports every close here; a close from one that does not is believed all the same."`
	// Logins are the hub logins this runner has from this hub (decision
	// 0055), each repeated in every sync until one carrying its terminal
	// state is answered with a 2xx. A login a runner reported and then leaves
	// out while it was not over is gone: the runner restarted, and a login in
	// flight does not survive a restart. Sent by runners that advertise the
	// "login" feature.
	Logins []LoginReport `json:"logins,omitempty" doc:"The hub logins this runner has from this hub, each repeated in every sync until one carrying its terminal state is answered with a 2xx. Record them by login_id and take a repeat as the same news. A login the runner reported, not yet terminal, that a later sync leaves out is gone — the runner restarted, and a login in flight does not survive that — so end it failed. A report for a login_id you never started is news you may ignore. Sent by runners advertising the login feature."`
}

// ClosedSession is one session a runner has closed, and why.
type ClosedSession struct {
	SessionID string             `json:"session_id" doc:"The session, by the id the hub gave it."`
	Reason    SessionCloseReason `json:"reason" enum:"closed,closed_by_owner,expired,disk_pressure" doc:"closed: the hub asked, with close_session. closed_by_owner: the runner's owner closed it on the machine. expired: nothing ran in it for the owner's idle TTL. disk_pressure: the disk under the workdirs ran low and it was among the longest idle. Each means the same for routing: a new run needs a new session. A closed set for all of v1: a value outside it goes only to a hub that advertised, in hub_features, the feature adding it."`
	ClosedAt  time.Time          `json:"closed_at" doc:"When the runner closed it."`
}

// SessionCloseReason is why a runner closed a session. A hub may tell them
// apart to say what happened; every one of them means the same thing for
// routing — the session is gone, and a new run needs a new session.
type SessionCloseReason string

const (
	// SessionClosed — the hub asked, with the close_session control.
	SessionClosed SessionCloseReason = "closed"
	// SessionClosedByOwner — the runner's owner closed it on the machine
	// (`yad sessions close`).
	SessionClosedByOwner SessionCloseReason = "closed_by_owner"
	// SessionExpired — nothing ran in it for the owner's idle TTL.
	SessionExpired SessionCloseReason = "expired"
	// SessionDiskPressure — the disk under the workdirs fell below the
	// owner's floor, and this was among the longest idle.
	SessionDiskPressure SessionCloseReason = "disk_pressure"
)

// SessionCloseReasons lists the closed set, for the parity test.
func SessionCloseReasons() []SessionCloseReason {
	return []SessionCloseReason{SessionClosed, SessionClosedByOwner, SessionExpired, SessionDiskPressure}
}

// Health is the runner's state as a hub needs it for routing and alerting.
//
// Every field of it is computed on every sync, so each one is a read the
// runner can afford every few seconds, and the lists are capped: a health
// block that grows with the machine's uptime makes every sync slower for ever.
//
// Load, disk, spool and outbox depth are for dashboards, and optional in the
// document (decision 0047): a sync refused for one of them renews no lease, so
// a hub strict about a dashboard field would lose every run of a runner build
// that left it out. free_capacity is the one field here routing needs.
type Health struct {
	// Load is the machine's one-minute load average, as uptime(1) prints it —
	// the whole machine, not this runner's share, and not divided by CPU
	// count, which the capability document does not carry. 0 from a machine
	// this runner cannot read one from.
	Load          float64         `json:"load,omitempty" doc:"The machine's one-minute load average, as uptime(1) prints it: the whole machine's, not divided by CPU count. For dashboards: absent is 0, or a machine the runner cannot read one from, and a hub never refuses a sync for its absence."`
	FreeCapacity  Capacity        `json:"free_capacity" doc:"What this sync may be offered, already net of every run the runner holds: total bounds the whole response, and each by_harness figure bounds that harness independently. Offer up to it as sent; do not subtract the runs listed beside it."`
	DiskFreeBytes int64           `json:"disk_free_bytes,omitempty" doc:"Free bytes on the disk under the runner's workdirs. For dashboards: absent is 0 or unknown, and a hub never refuses a sync for its absence."`
	Harnesses     []HarnessHealth `json:"harnesses,omitempty" doc:"Readiness of each harness the runner can drive, with its accounts. A harness with ready false is not claimed for: an offer for it comes back unlisted."`
	SpoolDepth    int             `json:"spool_depth,omitempty" doc:"Events the runner holds that this hub has not acknowledged. For dashboards: absent is 0 or unknown, and a hub never refuses a sync for its absence."`
	OutboxDepth   int             `json:"outbox_depth,omitempty" doc:"Results the runner holds that this hub has not acknowledged. For dashboards: absent is 0 or unknown, and a hub never refuses a sync for its absence."`
	// RecentErrors are the runner's own recent warnings and errors, newest
	// first, each "<RFC3339 time> <LEVEL> <message>" — why this runner is
	// slow or idle, in the words its owner sees in `yad status`.
	//
	// The runner's words and only those: the message of a log record, never
	// the key=value attrs beside it, which is where a wrapped error's text, a
	// path on the machine and anything a harness printed live. Bounded in
	// number, in age and in length by the runner.
	RecentErrors []string `json:"recent_errors,omitempty" doc:"The runner's recent warnings and errors, newest first, each '<RFC3339 time> <LEVEL> <message>'. The runner's own words: never what a harness printed, never a path on the machine, never a credential. Capped in number, age and length."`
	// Draining says the runner has stopped claiming and exits once the runs
	// it holds have ended. It keeps syncing until then, so their leases renew
	// and their results land; its free capacity is zero, and a hub offers it
	// nothing. Sent by runners that advertise the "drain" feature, which also
	// act on the drain control.
	Draining bool `json:"draining,omitempty" doc:"The runner has stopped claiming and exits once the runs it holds have ended. It keeps syncing until then; offer it nothing. The answer to a drain control, which stops repeating once this is true."`
}

// HarnessHealth is per-harness readiness with each account's state: limited
// until a reset, needing a login the owner has to finish, or free.
//
// Ready is whether the harness can take a run now — at least one free account,
// or none configured at all, since a harness with no accounts runs on the
// harness's own login. It is not a claim about the binary being installed;
// only harnesses the runner can drive appear here.
type HarnessHealth struct {
	ID       string          `json:"id" doc:"The harness id."`
	Ready    bool            `json:"ready" doc:"Whether the harness can take a run now: at least one account is free, or it has no accounts and runs on its own login."`
	Accounts []AccountReport `json:"accounts,omitempty" doc:"Each account's state, limit and usage windows as of this sync."`
}

// HeldRun is one run's non-terminal state. Terminal states travel in a Result.
type HeldRun struct {
	RunID     string     `json:"run_id" doc:"The run."`
	State     RunState   `json:"state" enum:"claimed,preparing,running,waiting" doc:"Where the run is. A run whose result is written but not yet acknowledged stays listed as running, so its lease outlasts a hub outage. A closed set for all of v1: a value outside it goes only to a hub that advertised, in hub_features, the feature adding it."`
	ResumesAt *time.Time `json:"resumes_at,omitempty" doc:"For a waiting run, the earliest moment it continues: the soonest reset among its harness's accounts. It may continue sooner if an account frees."`
	Reason    string     `json:"reason,omitempty" doc:"Why the run is in this state, in the runner's words, for display. Not a closed set."`
}

// SyncResponse is what the hub wants done until the next sync.
type SyncResponse struct {
	NextSyncMS int `json:"next_sync_ms" doc:"Milliseconds until the next sync, 5000 to 60000 inclusive. A runner clamps a value outside that range, adds its own jitter, and syncs sooner when this response offered runs."`
	// LeaseMS covers offers as well as claims (decision 0046): a runner that
	// goes silent holding an offer strands nothing past it.
	LeaseMS    int       `json:"lease_ms" doc:"How long the hub holds each run named in this answer for this runner without hearing from it, measured on the hub from when it handled this sync: every run offered here, and every run the request listed. A listed run the lease lapses on is lost, except a claim the hub has asked to cancel, which ends cancelled: the runner may have withdrawn it unstarted. An offered run the lease lapses on, never claimed, goes back in the queue and may be offered to any runner; a claim listed after that is answered with a cancel. Never shorter than next_sync_ms."`
	Runs       []Run     `json:"runs,omitempty" doc:"New runs offered. Not claimed yet: a run is claimed when the next sync lists it. Never more than the request's free_capacity, in total or for any harness."`
	Controls   []Control `json:"controls,omitempty" doc:"Instructions for the runner. Nothing acknowledges one: cancel and interrupt are repeated in every response while the run is listed, drain until health says draining, close_session until the session is in closed_sessions, remove_account until neither the capability document nor health lists the account; a steer is sent once."`
	MinVersion string    `json:"min_version,omitempty" doc:"The oldest yad this hub takes, as in the register response. Absent is no floor."`
}

// ControlKind is the closed set of instructions a hub can give in a sync.
type ControlKind string

const (
	ControlCancel             ControlKind = "cancel"
	ControlInterrupt          ControlKind = "interrupt"
	ControlSteer              ControlKind = "steer"
	ControlCloseSession       ControlKind = "close_session"
	ControlDrain              ControlKind = "drain"
	ControlReportCapabilities ControlKind = "report_capabilities"
	// ControlUpdate is reserved (decision 0018): a runner that does not
	// implement self-update ignores it and keeps reporting its version.
	ControlUpdate ControlKind = "update"
	// The hub-login controls (decision 0055), added within v1 and sent only
	// to a runner advertising the "login" feature, as every kind added to
	// the set is.
	ControlStartLogin  ControlKind = "start_login"
	ControlLoginCode   ControlKind = "login_code"
	ControlLoginToken  ControlKind = "login_token"
	ControlCancelLogin ControlKind = "cancel_login"
	// ControlRemoveAccount takes an account off the runner (decision 0057),
	// sent only to a runner advertising the "accounts" feature.
	ControlRemoveAccount ControlKind = "remove_account"
)

// ControlKinds lists the closed set, for validation and the parity test.
func ControlKinds() []ControlKind {
	return []ControlKind{ControlCancel, ControlInterrupt, ControlSteer, ControlCloseSession, ControlDrain, ControlReportCapabilities, ControlUpdate,
		ControlStartLogin, ControlLoginCode, ControlLoginToken, ControlCancelLogin, ControlRemoveAccount}
}

// Control is one instruction. Which of the optional fields are set depends on
// the kind: runs for cancel/interrupt/steer, sessions for close_session, none
// for drain — which a hub sends only to a runner advertising the "drain"
// feature, and repeats until the runner's health says it is draining.
//
// close_session goes only to a runner advertising the "close_session"
// feature, and is repeated until the session appears in the runner's
// closed_sessions. A session with a run held closes once that run ends; a
// session the runner does not hold, or already closed, is reported closed.
//
// steer and interrupt are gated the same way, on the "steer" and "interrupt"
// features. Only cancel and report_capabilities go to every v1 runner; update
// is reserved and goes to none. Nothing acknowledges a control, so one sent to
// a runner that does not act on it is indistinguishable, to whoever asked for
// it, from one that was obeyed.
//
// start_login, login_code, login_token and cancel_login are hub login
// (decision 0055), and go only to a runner advertising the "login" feature.
// Each is repeated until the runner's logins answer it: start_login and
// login_token until the login is reported, login_code while it is reported
// waiting, cancel_login until it is reported over. A runner takes a repeat as
// the same instruction.
//
// add on start_login or login_token, and remove_account, change which
// accounts the runner has (decision 0057), and go only to a runner advertising
// the "accounts" feature — which it advertises only to a hub its owner lets
// do so, and only beside "login". An add creates the account once its login
// takes; remove_account is repeated until neither the runner's capability
// document nor its health lists the account — the document decides, since
// health names only the harnesses a runner can drive and at most sixteen
// accounts of each — or the runner no longer advertises "accounts".
//
// Code and Token are secrets. Neither side logs them, stores them past their
// use or reports them back; a hub holds a token only until a sync from the
// runner reports the login it was delivered for, and never shows it again.
type Control struct {
	Kind      ControlKind `json:"kind" enum:"cancel,interrupt,steer,close_session,drain,report_capabilities,update,start_login,login_code,login_token,cancel_login,remove_account" doc:"cancel: end the run. interrupt: end the run's current turn and keep its session. steer: add text to the running turn. close_session: close the session and reclaim its workdir. drain: take no new runs and exit once the held ones end. report_capabilities: send the capability document in the next sync. update: reserved, never sent. start_login: log an account in by link, reporting the URL in logins (and for Codex the user_code to type there). login_code: the code the owner got at that URL, never for a login reporting user_code. login_token: store a token as the account's login. cancel_login: end a login. remove_account: take the account named by harness and account off the runner. interrupt, steer, close_session and drain go only to a runner advertising the feature of the same name; the four login kinds only to one advertising login; remove_account, and start_login or login_token carrying add, only to one advertising accounts. A closed set for all of v1: a kind outside it goes only to a runner that advertised, in protocol_features, the feature adding it."`
	RunID     string      `json:"run_id,omitempty" doc:"The run, for cancel, interrupt and steer."`
	SessionID string      `json:"session_id,omitempty" doc:"The session, for close_session."`
	Text      string      `json:"text,omitempty" doc:"What to tell the harness, for steer."`
	LoginID   string      `json:"login_id,omitempty" doc:"The login, for start_login, login_code, login_token and cancel_login: chosen by the hub, unique on it, 1-128 letters, digits, dots, dashes or underscores."`
	// Harness and Account name what to log in, or what remove_account takes
	// off. Without Add the account must already be listed on the runner.
	Harness string `json:"harness,omitempty" doc:"The harness, for start_login, login_token and remove_account: claude or codex. Codex logs in by device code, and never by token."`
	Account string `json:"account,omitempty" doc:"The account label, for start_login, login_token and remove_account. Without add, one the runner already lists; with add, a new one: lowercase letters, digits, dashes and underscores, starting with a letter or digit, at most 64. Absent in start_login logs in the harness's own default login; login_token, add and remove_account need one."`
	// Add makes a login create its account (decision 0057): listed on the
	// runner only once the login takes, so one that does not take lists
	// nothing.
	Add   bool   `json:"add,omitempty" doc:"For start_login and login_token: create the account named in account, listing it on the runner once its login takes. A login that does not take lists nothing. A label the runner already lists ends the login failed; log it in again without add. Sent only to a runner advertising accounts."`
	Code  string `json:"code,omitempty" doc:"For login_code: what the owner got after signing in at the login's url. A secret: never log it or show it again."`
	Token string `json:"token,omitempty" doc:"For login_token: a claude setup-token token. A secret that works for a year: never log it, never return it from any API, and hold it only until a sync from the runner reports this login."`
}
