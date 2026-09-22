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
	Err    v1.Error `json:"error" doc:"What went wrong. Every error response under the base carries this envelope, and next_action is always set."`
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

// huma produces its own errors — validation and bad JSON — through
// NewErrorWithContext. Requests that match no operation never reach huma;
// protocolRoutes and serviceRoutes answer those. Replacing it is huma's
// documented extension point, and it is process-wide, so the one hook serves
// both APIs: serviceRoutes marks its requests, and each API's errors point at
// its own document — a service caller never needs the runner protocol's.
func init() {
	// An absent list is an empty list on the wire; "array or null" would make
	// every TypeScript consumer handle a case that carries no meaning.
	huma.DefaultArrayNullable = false
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		return newError(false, status, msg, errs...)
	}
	huma.NewErrorWithContext = func(ctx huma.Context, status int, msg string, errs ...error) huma.StatusError {
		return newError(ctx != nil && ctx.Context().Value(serviceKey{}) != nil, status, msg, errs...)
	}
}

// serviceKey marks a request to the service API.
type serviceKey struct{}

func newError(service bool, status int, msg string, errs ...error) *ErrorResponse {
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
	if service {
		next = "fix the request to match protocol/hubapi/openapi.yaml"
	}
	// huma answers a body that parses and fails the schema with 422. HUB.md
	// names 400 invalid for every body that does not validate, and the
	// protocol document declares no 422 (protocolOp), so the protocol says 400.
	// The service API is this hub's own, and its document declares the 422 it
	// sends.
	if status == http.StatusUnprocessableEntity && !service {
		status = http.StatusBadRequest
	}
	switch status {
	case http.StatusRequestEntityTooLarge:
		// The status is what a runner acts on (HUB.md §6): it halves an
		// events batch and tries again, whatever the code says.
		if !service {
			next = "send less in one request: a runner halves an events batch and tries again"
		}
	case http.StatusUnauthorized:
		code, next = v1.CodeUnauthorized, "register again with a fresh registration token"
		if service {
			next = newAdminTokenAction
		}
	case http.StatusNotFound:
		code, next = v1.CodeNotFound, "check the connection URL"
		if service {
			next = "check the hub URL and the path"
		}
	case 0:
		// huma calls NewError(0, "") only to learn the error schema.
	default:
		if status >= 500 {
			code, next = v1.CodeInternal, "retry later; if it persists, the hub's logs have the cause"
		}
	}
	return Fail(status, code, msg, next)
}
