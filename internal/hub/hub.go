// Package hub is `yad hub`: the standalone hub, and the reference
// implementation of the server half of the v1 protocol.
//
// It serves three roles, which is why it lives in this repository: a hub for
// services that would rather not embed the protocol (decision 0003), the fake
// the runner's own tests run against, and the source of protocol/v1/openapi.yaml
// — the operations below are declared over the protocol/v1 types, and the
// document is generated from them (decision 0017).
//
// Register and sync are served; events, result and deregister are declared and
// documented, and answer not_implemented until the rest of epic E2 (Zumino
// yad/dev) lands.
//
// Beside the protocol, under hubapi.BasePath, is the service API — submit a
// run, read it, long-poll its events — which only this hub has, behind its own
// admin tokens (decision 0022).
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub/store"
)

// BasePath is where `yad hub` mounts the protocol. Other hubs choose their own
// base; every path in the protocol is relative to it.
const BasePath = "/v1"

// Timings. The hub owns them (ARCHITECTURE.md §2): a runner syncs at the
// interval the hub names, within these bounds, and a lease lapses after four
// missed intervals, so one dropped request never loses a run. It is never
// shorter than minLease, which outlasts the runner's backoff (1 s doubling to
// 30 s) through five failures in a row — a hub blip of about a minute.
const (
	DefaultSyncInterval = 15 * time.Second
	MinSyncInterval     = 5 * time.Second
	MaxSyncInterval     = 60 * time.Second
	missedIntervals     = 4
	minLease            = 60 * time.Second
)

// Options configure a hub. Store is required to serve; generating the OpenAPI
// document needs none.
type Options struct {
	Store *store.Store
	// SyncInterval is what runners are told; zero means DefaultSyncInterval.
	SyncInterval time.Duration
	// Now is the clock, replaced in tests so a lease can lapse without a sleep.
	Now func() time.Time
}

// Hub is the protocol server.
type Hub struct {
	api      huma.API
	service  huma.API
	bell     bell
	mux      *http.ServeMux
	store    *store.Store
	now      func() time.Time
	interval time.Duration
	lease    time.Duration
}

// New builds a hub with every v1 operation registered.
func New(opts Options) *Hub {
	h := &Hub{store: opts.Store, now: opts.Now, interval: opts.SyncInterval}
	if h.now == nil {
		h.now = time.Now
	}
	if h.interval == 0 {
		h.interval = DefaultSyncInterval
	}
	h.interval = min(max(h.interval, MinSyncInterval), MaxSyncInterval)
	h.lease = max(missedIntervals*h.interval, minLease)

	inner := http.NewServeMux()
	api := humago.New(inner, Config())
	h.register(api)

	svcMux := http.NewServeMux()
	svc := humago.New(svcMux, ServiceConfig())
	h.registerService(svc)

	outer := http.NewServeMux()
	outer.Handle(BasePath+"/", http.StripPrefix(BasePath, protocolRoutes(inner)))
	outer.Handle(hubapi.BasePath+"/", http.StripPrefix(hubapi.BasePath, h.serviceRoutes(svcMux)))
	// Anything outside the base is a wrong connection URL. Say so in the
	// protocol's own shape, not the mux's plain-text 404.
	outer.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, Fail(http.StatusNotFound, v1.CodeNotFound, "no protocol at "+r.URL.Path,
			"the connection URL must end in the hub's base, e.g. https://host"+BasePath))
	})
	h.api, h.service, h.mux = api, svc, outer
	return h
}

// protocolRoutes makes every response under the base the protocol's own.
// huma sees only requests that match an operation, so a path or method that
// matches none would otherwise get the mux's plain-text 404 or 405, which a
// runner can only report as "is this even a hub?". And a request speaking
// another protocol version is refused before its body is decoded — a body
// shaped for v2 fails v1 validation in ways that say nothing about the cause.
func protocolRoutes(ops *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The version is checked before the route: a newer runner calling an
		// operation this version does not have must hear "version mismatch",
		// not "check your URL".
		if r.Method == http.MethodPost && r.Header.Get(v1.HeaderProtocol) != v1.Version {
			writeError(w, Fail(http.StatusUpgradeRequired, v1.CodeUnsupportedProtocol,
				"this hub speaks protocol "+v1.Version+"; the request said "+strconv.Quote(r.Header.Get(v1.HeaderProtocol)),
				"upgrade yad or the hub so both speak protocol "+v1.Version))
			return
		}
		// huma hands operations the request's context and nothing else of the
		// request, so the bearer travels there. Reading it this way rather than
		// as a declared header parameter keeps it out of the operations'
		// parameters: openapi.yaml already declares it as the security scheme.
		r = r.WithContext(context.WithValue(r.Context(), bearerKey{}, bearerFrom(r)))
		if unmatched(ops, w, r, "every protocol call is a POST — see protocol/v1/openapi.yaml",
			"check the connection URL and the path against protocol/v1/openapi.yaml") {
			return
		}
		ops.ServeHTTP(w, r)
	})
}

// serviceRoutes guards the service API: an admin token before anything else,
// so an unauthenticated caller learns nothing about which paths exist, and the
// same error envelope as the protocol for every path and method.
func (h *Hub) serviceRoutes(ops *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h.authenticateAdmin(r.Context(), bearerFrom(r)); err != nil {
			var e *ErrorResponse
			if !errors.As(err, &e) {
				e = Fail(http.StatusInternalServerError, "internal", "the hub could not check the admin token", "retry later; if it persists, the hub's logs have the cause")
			}
			writeError(w, e)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), serviceKey{}, true))
		if unmatched(ops, w, r, "see protocol/hubapi/openapi.yaml for each path's method",
			"check the hub URL and the path against protocol/hubapi/openapi.yaml") {
			return
		}
		ops.ServeHTTP(w, r)
	})
}

// unmatched answers a request no operation matches with the error envelope,
// and reports whether it did. huma sees only requests that match, so the mux's
// own plain-text 404 or 405 would otherwise reach the caller.
func unmatched(ops *http.ServeMux, w http.ResponseWriter, r *http.Request, methodHint, pathHint string) bool {
	fallback, pattern := ops.Handler(r)
	if pattern != "" {
		return false
	}
	rec := &recorder{header: http.Header{}, status: http.StatusNotFound}
	fallback.ServeHTTP(rec, r)
	if rec.status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", rec.header.Get("Allow"))
		writeError(w, Fail(http.StatusMethodNotAllowed, v1.CodeInvalid, r.Method+" "+r.URL.Path+" is not an operation", methodHint))
		return true
	}
	writeError(w, Fail(http.StatusNotFound, v1.CodeNotFound, "no operation at "+r.URL.Path, pathHint))
	return true
}

// recorder captures what the mux's own fallback would have written, so its
// status and Allow header can be carried into a protocol error.
type recorder struct {
	header http.Header
	status int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(status int)      { r.status = status }

// ServeHTTP serves the protocol under BasePath.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// OpenAPI returns the protocol's generated document as YAML.
func (h *Hub) OpenAPI() ([]byte, error) { return h.api.OpenAPI().YAML() }

// ServiceOpenAPI returns the service API's generated document as YAML.
func (h *Hub) ServiceOpenAPI() ([]byte, error) { return h.service.OpenAPI().YAML() }

// Config is the OpenAPI frame. Its version is the protocol's, never the
// binary's, so a yad release that changes no wire type changes no byte of the
// committed document.
func Config() huma.Config {
	c := huma.DefaultConfig("YAD runner protocol", v1.Version+".0.0")
	c.Info.Description = "The protocol between a YAD runner and a hub. " +
		"Runners call out; hubs never call in. Paths are relative to the connection's base URL."
	c.Servers = []*huma.Server{{URL: BasePath, Description: "where `yad hub` mounts it; other hubs choose their own base"}}
	// No docs UI and no schema-link headers: yad hub is headless (0003), and a
	// $schema field in every response is noise a TypeScript hub would copy.
	c.DocsPath = ""
	c.SchemasPath = ""
	c.OpenAPIPath = "/openapi"
	c.CreateHooks = nil
	// Within v1 either side may add fields; a strict "additionalProperties:
	// false" would make every TypeScript hub reject the next minor addition.
	c.AllowAdditionalPropertiesByDefault = true
	c.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"runner": {Type: "http", Scheme: "bearer", Description: "The runner credential; the registration token on register only."},
	}
	return c
}

// protocolHeader is on every request so a hub can refuse a version it does not
// host before decoding the body. protocolRoutes enforces it with a 426; the
// declaration here is what puts it in openapi.yaml.
type protocolHeader struct {
	Protocol string `header:"Yad-Protocol" required:"true" enum:"1" doc:"The protocol major version."`
}

type (
	registerInput struct {
		protocolHeader
		Body v1.RegisterRequest
	}
	registerOutput struct{ Body v1.RegisterResponse }

	syncInput struct {
		protocolHeader
		Runner string `path:"runner" doc:"The runner id."`
		Body   v1.SyncRequest
	}
	syncOutput struct{ Body v1.SyncResponse }

	eventsInput struct {
		protocolHeader
		Run  string `path:"run" doc:"The run id."`
		Body v1.EventBatch
	}
	eventsOutput struct{ Body v1.EventAck }

	resultInput struct {
		protocolHeader
		Run  string `path:"run" doc:"The run id."`
		Body v1.Result
	}
	deregisterInput struct {
		protocolHeader
		Runner string `path:"runner" doc:"The runner id."`
		Body   v1.DeregisterRequest
	}
	ackOutput struct{ Body v1.Ack }
)

var security = []map[string][]string{{"runner": {}}}

func (h *Hub) register(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "register", Method: http.MethodPost, Path: "/runners/register",
		Summary: "Exchange a registration token for a runner credential",
		Description: "Called once by `yad connect`, with the one-time registration token as the bearer. The token is dead afterwards. " +
			"Registering a runner id the hub already knows needs a token issued for that runner, and replaces its credential; " +
			"the old one stops working. With a token for a new runner it is refused with 409.",
		Security: security, Errors: []int{400, 401, 409, 426},
	}, h.registerRunner)

	huma.Register(api, huma.Operation{
		OperationID: "sync", Method: http.MethodPost, Path: "/runners/{runner}/sync",
		Summary: "Heartbeat, lease renewal, health and the ask for work, in one call",
		Description: "Every run listed is claimed or has its lease renewed. A run offered in the previous response and not listed " +
			"here was never received and will be offered again. A listed run this runner does not hold — never offered to it, " +
			"offered to another, or already finished or lost — is answered with a cancel control for that run.",
		Security: security, Errors: []int{400, 401, 403, 426},
	}, h.sync)

	huma.Register(api, huma.Operation{
		OperationID: "appendEvents", Method: http.MethodPost, Path: "/runs/{run}/events",
		Summary:     "Append a batch of run events",
		Description: "Idempotent by (run, seq). The response's acked_through is authoritative; the runner resends everything after it.",
		Security:    security, Errors: []int{400, 401, 404, 426},
	}, func(ctx context.Context, in *eventsInput) (*eventsOutput, error) {
		return nil, notYet("appendEvents")
	})

	huma.Register(api, huma.Operation{
		OperationID: "submitResult", Method: http.MethodPost, Path: "/runs/{run}/result",
		Summary:     "Report a run's terminal state",
		Description: "Retried from the runner's outbox until acknowledged. 409 means the hub already holds a different terminal state, which wins.",
		Security:    security, Errors: []int{400, 401, 404, 409, 426},
	}, func(ctx context.Context, in *resultInput) (*ackOutput, error) {
		return nil, notYet("submitResult")
	})

	huma.Register(api, huma.Operation{
		OperationID: "deregister", Method: http.MethodPost, Path: "/runners/{runner}/deregister",
		Summary:     "Retire this runner's credential",
		Description: "Runs the runner still holds become lost on the hub's side.",
		Security:    security, Errors: []int{401, 426},
	}, func(ctx context.Context, in *deregisterInput) (*ackOutput, error) {
		return nil, notYet("deregister")
	})
}

func notYet(op string) error {
	return Fail(http.StatusNotImplemented, v1.CodeNotImplemented,
		"yad hub does not implement "+op+" yet",
		"the hub's behaviour arrives in epic E2 (Zumino yad/dev) — see ARCHITECTURE.md §9")
}

// bearerFrom returns the request's bearer secret, or "" when it has none.
func bearerFrom(r *http.Request) string {
	scheme, secret, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(secret)
}

type bearerKey struct{}

func bearer(ctx context.Context) string {
	s, _ := ctx.Value(bearerKey{}).(string)
	return s
}

func writeError(w http.ResponseWriter, e *ErrorResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(e)
}
