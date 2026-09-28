package hubapi

import (
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// LoginRequest starts a hub login on a runner (decision 0055): by link when
// Token is empty — the runner runs the harness's own login and reports the
// link to sign in at — and by token when it is set.
type LoginRequest struct {
	LoginID string `json:"login_id,omitempty" doc:"Chosen by the caller, 1-128 letters, digits, dots, dashes or underscores; generated when absent. An id the hub already has is refused with 409."`
	Harness string `json:"harness" minLength:"1" doc:"The harness to log in: claude or codex. Codex logs in by device code, and never by token."`
	// Account empty is the harness's own default login, which only the link
	// way can log in: a token is always an account's (decision 0054).
	Account string `json:"account,omitempty" doc:"The account label, one the runner's owner listed on that machine: a hub never adds an account. Absent logs in the harness's own default login, by link only."`
	// Token is the one secret in the service API that travels inwards.
	Token string `json:"token,omitempty" doc:"A claude setup-token token, to log the account in by token instead of by link. A secret: sent to the runner once, blanked in the hub's store as soon as the runner reports the login, and never returned by this API."`
}

// LoginCodeRequest is the code the owner got after signing in at a link
// login's URL.
type LoginCodeRequest struct {
	Code string `json:"code" minLength:"1" maxLength:"4096" doc:"What the provider showed after signing in at the login's url. Delivered at the runner's next sync, and never returned by this API."`
}

// LoginState is where a hub login is: requested, before its runner has taken
// it, and then the runner's own states.
type LoginState string

// LoginRequested waits for the runner's next sync.
const LoginRequested LoginState = "requested"

// Terminal reports whether a login in state s is over.
func (s LoginState) Terminal() bool { return v1.LoginState(s).IsTerminal() }

// Login is a hub login as a service sees it. The code and the token are never
// included.
type Login struct {
	LoginID  string         `json:"login_id" doc:"The login id."`
	RunnerID string         `json:"runner_id" doc:"The runner the login is on."`
	Harness  string         `json:"harness" doc:"The harness being logged in."`
	Account  string         `json:"account,omitempty" doc:"The account label. Absent for the harness's own default login."`
	Method   v1.LoginMethod `json:"method" enum:"link,token" doc:"link: the runner runs the harness's own login and reports its url; send the code with POST .../code — unless it also reports user_code (Codex's device code), which is typed at url and never sent. token: the token sent at start is stored as the account's login."`
	State    LoginState     `json:"state" enum:"requested,starting,waiting,checking,succeeded,failed,expired,cancelled" doc:"requested: the runner hears of it at its next sync. Then the runner's own: starting, waiting (url is ready; send the code — or, with user_code, have it typed at url and send none), checking, and the four ends — succeeded, failed, expired, cancelled."`
	// URL is what the runner reported the harness printed: data, to show.
	URL      string `json:"url,omitempty" doc:"Where to sign in, while waiting: the harness's own authorize link, as the runner reported it."`
	UserCode string `json:"user_code,omitempty" doc:"For a device-code login — Codex's — the code to type at url. Nothing comes back through the hub: such a login takes no code, and ends by the runner's own check."`
	Error    string `json:"error,omitempty" doc:"Why the login did not take, with what to do, in the runner's words — or the hub's, when it never reached the runner or the runner stopped reporting it."`
	// CancelRequestedAt is set by a cancel until the runner reports the end.
	CancelRequestedAt *time.Time `json:"cancel_requested_at,omitempty" doc:"When a cancel was asked for; present until the login ends."`
	// CodeSent says a code is waiting for the runner's next sync.
	CodeSent  bool      `json:"code_sent,omitempty" doc:"A code was sent and the runner has not yet reported taking it."`
	CreatedAt time.Time `json:"created_at" doc:"When the login was started."`
	UpdatedAt time.Time `json:"updated_at" doc:"When its state last changed."`
}
