package v1

import "time"

// A hub login (decision 0055) is an account's login started from a hub: by
// link, where the runner runs the harness's own login and reports its URL,
// and the hub hands back the code the owner was given; or by token, where the
// hub delivers a `claude setup-token` token once. Either way the credential
// ends up on the machine and the runner's own login check decides whether it
// took. Every control and field here goes only to and from a runner
// advertising the "login" feature.

// LoginMethod is how a hub login proceeds.
type LoginMethod string

const (
	// LoginByLink runs the harness's own login on the machine; the owner
	// follows the URL it prints and the hub sends back the code.
	LoginByLink LoginMethod = "link"
	// LoginByToken stores a token the owner pasted into the hub.
	LoginByToken LoginMethod = "token"
)

// LoginMethods lists the closed set, for the parity test.
func LoginMethods() []LoginMethod { return []LoginMethod{LoginByLink, LoginByToken} }

// LoginState is where a hub login is on the runner.
type LoginState string

const (
	// LoginStarting is taken: the harness's login is starting, or a token
	// is being stored.
	LoginStarting LoginState = "starting"
	// LoginWaiting has a URL, and waits for the code the owner gets there —
	// or, for a device-code login, for the owner to type its user code there.
	LoginWaiting LoginState = "waiting"
	// LoginChecking has its code or token, and is asking the harness
	// whether the login took.
	LoginChecking LoginState = "checking"
	// LoginSucceeded took: the harness's own check says the home is
	// logged in, and the account takes runs.
	LoginSucceeded LoginState = "succeeded"
	// LoginFailed did not take; error says why and what to do.
	LoginFailed LoginState = "failed"
	// LoginExpired waited for a code longer than the runner allows.
	LoginExpired LoginState = "expired"
	// LoginCancelled was ended by cancel_login, or by a newer login for the
	// same account.
	LoginCancelled LoginState = "cancelled"
)

// LoginStates lists the closed set, for the parity test.
func LoginStates() []LoginState {
	return []LoginState{LoginStarting, LoginWaiting, LoginChecking, LoginSucceeded, LoginFailed, LoginExpired, LoginCancelled}
}

// IsTerminal reports whether a login in state s is over.
func (s LoginState) IsTerminal() bool {
	switch s {
	case LoginSucceeded, LoginFailed, LoginExpired, LoginCancelled:
		return true
	}
	return false
}

// LoginReport is one hub login as the runner has it, repeated in every sync
// until a sync carrying its terminal state is answered with a 2xx — the
// closed_sessions pattern (decision 0035).
//
// Nothing in it is a secret: the code and the token travel only towards the
// runner, and are never reported back.
type LoginReport struct {
	LoginID string `json:"login_id" doc:"The login, by the id the hub gave it in start_login or login_token."`
	// Harness, Account and Method are absent only from a report about a login
	// the runner never had — the answer to a login_code or cancel_login for
	// an id it does not know, which it has no way to fill in.
	Harness string      `json:"harness,omitempty" doc:"The harness being logged in. Absent only when the runner never had this login: the answer to a login_code or cancel_login for an id it does not know."`
	Account string      `json:"account,omitempty" doc:"The account label, as the owner listed it on the machine. Absent for the harness's own default login, and when the runner never had this login."`
	Method  LoginMethod `json:"method,omitempty" enum:"link,token" doc:"link: the runner ran the harness's own login and reports its URL. token: the runner stored a token the hub delivered. Absent only when the runner never had this login. A closed set for all of v1."`
	State   LoginState  `json:"state" enum:"starting,waiting,checking,succeeded,failed,expired,cancelled" doc:"starting: taken, and not yet waiting. waiting: url (and for a device-code harness user_code) is ready; send the code with login_code, unless user_code is set: then the owner types user_code at url, and no code is sent. checking: the code or token is in, or the device code was entered, and the runner is asking the harness whether it took. succeeded: the harness's own check says the account is logged in, and it takes runs. failed: it did not take, and error says why. expired: no code arrived, or the device code was not entered, within the runner's limit (ten minutes for yad). cancelled: ended by cancel_login, or by a newer login for the same account. The last four are terminal. A closed set for all of v1."`
	// URL is harness output, and data: it is shown to the owner, never
	// followed by the hub.
	URL string `json:"url,omitempty" doc:"Where the owner signs in, while waiting: the harness's own authorize URL, as it printed it. Show it; never follow it. Absent until waiting, and for a token login."`
	// UserCode is for a device-code harness — Codex's login gives a URL and a
	// code to type there, and no code comes back (decision 0057). Unused by
	// Claude.
	UserCode string `json:"user_code,omitempty" doc:"For a device-code login — Codex's — while waiting: the code the owner types at url. Show it beside url; nothing comes back through the hub, so send no login_code for a login that has one. Absent for Claude, whose login instead waits for login_code."`
	Error    string `json:"error,omitempty" doc:"Why a login failed, expired or was cancelled, in the runner's words, with the next action. Never a path on the machine, never a credential."`
	// UpdatedAt is the runner's clock, when the state last moved.
	UpdatedAt time.Time `json:"updated_at" doc:"When the login last changed state, by the runner's clock."`
}
