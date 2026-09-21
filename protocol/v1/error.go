package v1

import "fmt"

// Error is the body of every non-2xx response. NextAction is mandatory: a
// runner on a customer's machine is debugged by someone reading it.
type Error struct {
	Code       string `json:"code" doc:"What went wrong, for a program to act on. v1 names ten codes: not_implemented, unauthorized, runner_revoked, version_too_old, conflict, not_found, invalid, unsupported_protocol, not_holder and internal. They are not a closed set: a client meeting a code it does not know acts on the HTTP status and shows message and next_action to a person."`
	Message    string `json:"message"`
	NextAction string `json:"next_action"`
}

// ErrorEnvelope wraps Error so the body is always {"error": {...}} and a hub
// can add sibling fields later.
type ErrorEnvelope struct {
	Error Error `json:"error"`
}

func (e *Error) Error() string {
	if e.NextAction == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s — %s", e.Code, e.Message, e.NextAction)
}

// Error codes a runner acts on. Anything else is shown to a human. Adding one
// is not a breaking change — Error.Code is a plain string on the wire, not an
// enum — so a name is added here and in HUB.md, never removed.
const (
	CodeNotImplemented      = "not_implemented"
	CodeUnauthorized        = "unauthorized"
	CodeRunnerRevoked       = "runner_revoked"
	CodeVersionTooOld       = "version_too_old"
	CodeConflict            = "conflict"
	CodeNotFound            = "not_found"
	CodeInvalid             = "invalid"
	CodeUnsupportedProtocol = "unsupported_protocol"
	// CodeNotHolder answers events or a result for a run the calling runner
	// does not hold — it lost the race for it, or never had it. Nothing it
	// sends for that run is applied, and no retry changes that.
	CodeNotHolder = "not_holder"
	// CodeInternal answers a fault on the hub's side: the request may have
	// been fine, and the same one later may succeed. It comes with a 5xx, and
	// a runner treats it as it treats any 5xx — the sync loop backs off, the
	// reporter keeps events and results spooled and sends them again — so it
	// is never final and never stops a runner. Named so a hub has a code to
	// send for it rather than inventing one.
	CodeInternal = "internal"
)
