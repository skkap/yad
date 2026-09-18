package v1

import "fmt"

// Error is the body of every non-2xx response. NextAction is mandatory: a
// runner on a customer's machine is debugged by someone reading it.
type Error struct {
	Code       string `json:"code"`
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

// Error codes a runner acts on. Anything else is shown to a human.
const (
	CodeNotImplemented      = "not_implemented"
	CodeUnauthorized        = "unauthorized"
	CodeRunnerRevoked       = "runner_revoked"
	CodeVersionTooOld       = "version_too_old"
	CodeConflict            = "conflict"
	CodeNotFound            = "not_found"
	CodeInvalid             = "invalid"
	CodeUnsupportedProtocol = "unsupported_protocol"
)
