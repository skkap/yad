package hub

import (
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	v1 "github.com/skkap/yad/protocol/v1"
)

// ErrorResponse is a protocol error as huma writes it: the body is exactly
// {"error": {"code", "message", "next_action"}}, the shape every hub must use,
// so the OpenAPI document declares it once for every operation.
type ErrorResponse struct {
	status int
	Err    v1.Error `json:"error"`
}

func (e *ErrorResponse) Error() string  { return e.Err.Error() }
func (e *ErrorResponse) GetStatus() int { return e.status }

// ContentType keeps protocol errors plain JSON rather than huma's default
// application/problem+json: a runner decodes one error shape, not two.
func (e *ErrorResponse) ContentType(string) string { return "application/json" }

// Fail builds a protocol error. The next action is mandatory: a runner on a
// customer's machine is debugged by whoever reads it.
func Fail(status int, code, message, next string) *ErrorResponse {
	return &ErrorResponse{status: status, Err: v1.Error{Code: code, Message: message, NextAction: next}}
}

// huma produces its own errors — validation and bad JSON — through NewError.
// Requests that match no operation never reach huma; protocolRoutes answers
// those. Replacing it is huma's documented extension point; it is
// process-wide, which is fine in a binary that only ever serves this protocol.
func init() {
	// An absent list is an empty list on the wire; "array or null" would make
	// every TypeScript consumer handle a case that carries no meaning.
	huma.DefaultArrayNullable = false
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		var details []string
		for _, e := range errs {
			if e != nil {
				details = append(details, e.Error())
			}
		}
		if len(details) > 0 {
			msg = msg + ": " + strings.Join(details, "; ")
		}
		code := v1.CodeInvalid
		next := "fix the request to match protocol/v1/openapi.yaml"
		switch status {
		case http.StatusUnauthorized:
			code, next = v1.CodeUnauthorized, "register again with a fresh registration token"
		case http.StatusNotFound:
			code, next = v1.CodeNotFound, "check the connection URL"
		case 0:
			// huma calls NewError(0, "") only to learn the error schema.
		default:
			if status >= 500 {
				code, next = "internal", "retry later; if it persists, the hub's logs have the cause"
			}
		}
		return Fail(status, code, msg, next)
	}
}
