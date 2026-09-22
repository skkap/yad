// Package hub is `yad hub`: the standalone hub, and the reference
// implementation of the server half of the v1 protocol.
//
// It serves three roles, which is why it lives in this repository: a hub for
// services that would rather not embed the protocol (decision 0003), the fake
// the runner's own tests run against, and the source of protocol/v1/openapi.yaml
// — the operations below are declared over the protocol/v1 types, and the
// document is generated from them (decision 0017).
//
// Every operation the v1 protocol has is served: register, sync, events,
// result and deregister.
//
// Beside the protocol, under hubapi.BasePath, is the service API — submit a
// run, read it, long-poll its events — which only this hub has, behind its own
// admin tokens (decision 0022).
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// DefaultAbandonAfter is how long a runner may go without a sync before the
// hub gives up its sessions (decision 0046). A day outlasts a laptop closed
// overnight, and not one closed for a weekend, and that is the trade: shorter
// strands fewer queued runs behind a machine that is gone for good, and longer
// closes fewer warm sessions on one that was only asleep. An operator whose
// runners sleep for days says so with --abandon-after.
const DefaultAbandonAfter = 24 * time.Hour

// SyncFloorForTests replaces MinSyncInterval in the clamp while it is positive,
// so a test that is both sides of the protocol can run it in milliseconds
// instead of waiting five seconds for every sync — at the shipped floor the
// end-to-end tests spend their time asleep, 201 s of a 315 s suite (DEV-63).
// It is deliberately not an Options field and not reachable from the protocol:
// timings are the hub's (ARCHITECTURE.md §2), and a runner must never be able
// to lower them. Nothing outside a test may set it: TestOnlyTestsReachTheSyncFloor
// fails on any shipped file of the module that assigns it, this one included.
// That is a backstop against the assignment someone would actually write, not a
// proof — reading the source cannot see a value reached some other way.
var SyncFloorForTests time.Duration

// LeaseForTests replaces the lease a hub names while it is positive, so a test
// can watch a lease lapse without spending a minute doing it — the conformance
// suite's lease rule is checked by waiting one out, against a hub over HTTP,
// with no clock in common (DEV-33).
//
// Deliberately a replacement and not a floor like the seam above: the lease is
// max(four intervals, minLease) and the interval is bounded below by five
// seconds (ARCHITECTURE.md §2), so no floor can bring a lease under twenty
// seconds and a floor would buy nothing. Same rules otherwise: nothing outside
// a test may set it, timings stay the hub's, and no runner can reach it.
// TestOnlyTestsReachTheLease fails on any shipped file of the module that
// assigns it, this one included.
var LeaseForTests time.Duration

// Options configure a hub. Store is required to serve; generating the OpenAPI
// document needs none.
type Options struct {
	Store *store.Store
	// SyncInterval is what runners are told; zero means DefaultSyncInterval.
	SyncInterval time.Duration
	// AbandonAfter is how long a runner may go without a sync before its
	// sessions are closed and the runs queued in them fail; zero means
	// DefaultAbandonAfter. Validate it with ValidateAbandonAfter: one no
	// longer than the lease would give up on a runner whose runs are still
	// leased to it.
	AbandonAfter time.Duration
	// MinVersion is the oldest yad an operator will support against this hub.
	// Empty is no floor, and is the default: a hub only refuses a runner when
	// its operator has said which version to refuse below. Validate it with
	// ValidateMinVersion — one the hub cannot parse is no floor.
	MinVersion string
	// Now is the clock, replaced in tests so a lease can lapse without a sleep.
	Now func() time.Time
	// Command builds the yad command an operator runs against this hub's own
	// database, for an answer's next action: the hub is handed a store, not
	// the profile or the --db that opened it, so only its caller can name
	// them. Nil, and the command carries placeholders for both.
	Command func(args ...string) string
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
	// abandonAfter is the silence after which a runner's sessions are
	// given up (decision 0046), and started is when this hub began counting
	// it.
	abandonAfter time.Duration
	started      time.Time
	// minVersion is the floor runners are refused below, and is sent to every
	// runner in the register and sync responses so it can say why it stopped.
	minVersion string
	command    func(args ...string) string
}

// New builds a hub with every v1 operation registered.
func New(opts Options) *Hub {
	h := &Hub{store: opts.Store, now: opts.Now, minVersion: opts.MinVersion, command: opts.Command, abandonAfter: opts.AbandonAfter}
	if h.now == nil {
		h.now = time.Now
	}
	h.interval, h.lease = timings(opts.SyncInterval)
	if h.abandonAfter == 0 {
		h.abandonAfter = DefaultAbandonAfter
	}
	h.started = h.now()

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

// timings is the interval a hub names for the one it was configured with, and
// the lease that goes with it.
func timings(configured time.Duration) (interval, lease time.Duration) {
	interval = configured
	if interval == 0 {
		interval = DefaultSyncInterval
	}
	floor := MinSyncInterval
	if SyncFloorForTests > 0 {
		floor = SyncFloorForTests
	}
	interval = min(max(interval, floor), MaxSyncInterval)
	lease = max(missedIntervals*interval, minLease)
	if LeaseForTests > 0 {
		lease = LeaseForTests
	}
	return interval, lease
}

// ValidateAbandonAfter refuses a silence no longer than the lease a hub with
// this sync interval names. Such a runner has not gone: its runs are still
// leased to it, its next sync may be on the way, and closing its sessions
// would fail the runs queued behind a runner doing everything right. Zero is
// refused too: it is Options' "the default", and an operator typing it more
// likely means "never", which this hub does not offer.
func ValidateAbandonAfter(abandonAfter, syncInterval time.Duration) error {
	_, lease := timings(syncInterval)
	if abandonAfter <= lease {
		return fmt.Errorf("--abandon-after %s is not longer than this hub's %s lease, and a runner whose runs are still leased to it has not gone — name more than %s (the default is %s)",
			abandonAfter, lease, lease, DefaultAbandonAfter)
	}
	return nil
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
				e = Fail(http.StatusInternalServerError, v1.CodeInternal, "the hub could not check the admin token", "retry later; if it persists, the hub's logs have the cause")
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

// OpenAPIYAML is the protocol's generated document, as the committed file
// holds it.
//
// The document is a property of the code, not of a hub instance, so it is not
// a method. constrainSources writes into the schema registry the document is
// made of — huma's document *is* its registry — so applying it to a hub that
// serves requests makes that hub validate against the oneOf and answer 422
// where Run.Validate should answer 400. Each of these builds a hub of its own
// for the purpose, unreachable and discarded, so no serving hub is ever
// touched.
//
// A method would be the obvious reach for anyone adding a spec endpoint, and
// would reintroduce exactly that. There is none to reach for, and
// TestConstrainSourcesIsReachedOnlyFromTheGenerators fails if one appears.
func OpenAPIYAML() ([]byte, error) { return constrainSources(New(Options{}).api.OpenAPI()).YAML() }

// ServiceOpenAPIYAML is the service API's generated document, on the same
// terms as OpenAPIYAML above.
func ServiceOpenAPIYAML() ([]byte, error) {
	return constrainSources(New(Options{}).service.OpenAPI()).YAML()
}

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
	// Not served, for the reason the other two are not and for one of its own:
	// the committed protocol/v1/openapi.yaml is the contract. A served
	// document is marshalled from the live schema registry, which deliberately
	// does not carry the Source oneOf — constraining it would make this hub
	// validate against it — so the served bytes would differ from the
	// committed bytes in exactly the rule a hub author came for. If a hub
	// should serve its document one day, it serves the committed file.
	c.OpenAPIPath = ""
	c.CreateHooks = nil
	// Within v1 either side may add fields; a strict "additionalProperties:
	// false" would make every TypeScript hub reject the next minor addition.
	c.AllowAdditionalPropertiesByDefault = true
	c.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"runner": {Type: "http", Scheme: "bearer", Description: "The runner credential; the registration token on register only."},
	}
	return c
}

// ProtocolHeader is on every request so a hub can refuse a version it does not
// host before decoding the body. protocolRoutes enforces it with a 426, before
// huma sees the request; embedding this in an operation's input is what
// declares it in protocol/v1/openapi.yaml, so a generated client sends it.
//
// It is exported only because huma skips unexported fields, and an embedded
// field takes its type's name: embedded as protocolHeader, it silently
// declared nothing (DEV-93). TestProtocolHeaderIsDeclaredWhereEnforced holds
// the declaration and the enforcement to each other.
type ProtocolHeader struct {
	Protocol string `header:"Yad-Protocol" required:"true" enum:"1" doc:"The protocol major version."`
}

type (
	registerInput struct {
		ProtocolHeader
		Body v1.RegisterRequest
	}
	registerOutput struct{ Body v1.RegisterResponse }

	syncInput struct {
		ProtocolHeader
		Runner string `path:"runner" doc:"The runner id."`
		Body   v1.SyncRequest
	}
	syncOutput struct{ Body v1.SyncResponse }

	eventsInput struct {
		ProtocolHeader
		Run  string `path:"run" doc:"The run id."`
		Body v1.EventBatch
	}
	eventsOutput struct{ Body v1.EventAck }

	resultInput struct {
		ProtocolHeader
		Run  string `path:"run" doc:"The run id."`
		Body v1.Result
	}
	deregisterInput struct {
		ProtocolHeader
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
			"here was never received and will be offered again. An offer carries the same lease_ms as a claim: one not claimed within it " +
			"goes back in the queue, for this runner or any other, and a claim listed after that is refused with a cancel, as a lapsed " +
			"claim is. A listed run this runner does not hold — never offered to it, offered to another, or already finished or lost — " +
			"is answered with a cancel control for that run. A runner that does not sync for longer than the hub's abandon-after " +
			"(yad hub: 24 h, --abandon-after) has every session bound to it closed and the runs queued in them ended, and keeps its " +
			"credential: its next sync is answered normally, with close_session for each of those sessions until it reports the close.",
		Security: security, Errors: []int{400, 401, 403, 426},
	}, h.sync)

	huma.Register(api, huma.Operation{
		OperationID: "appendEvents", Method: http.MethodPost, Path: "/runs/{run}/events",
		Summary: "Append a batch of run events",
		Description: "Idempotent by (run, seq): a resent event is ignored and the first copy stands. The response's acked_through " +
			"is the highest seq up to which the hub holds every event, and is authoritative; the runner resends everything after it. " +
			"Only the runner the run was claimed by may append, before or after it ends; any other gets 403 not_holder.",
		Security: security, Errors: []int{400, 401, 403, 404, 426},
		MaxBodyBytes: reportBodyLimit,
	}, h.appendEvents)

	huma.Register(api, huma.Operation{
		OperationID: "submitResult", Method: http.MethodPost, Path: "/runs/{run}/result",
		Summary: "Report a run's terminal state",
		Description: "Retried from the runner's outbox until acknowledged, and applied at most once: the same state again is acknowledged. " +
			"409 means the hub already holds a different terminal state — lost, when the lease lapsed first — which stands; the runner stops reporting. " +
			"Only the runner the run was offered to or claimed by may report; any other gets 403 not_holder.",
		Security: security, Errors: []int{400, 401, 403, 404, 409, 426},
		MaxBodyBytes: reportBodyLimit,
	}, h.submitResult)

	huma.Register(api, huma.Operation{
		OperationID: "deregister", Method: http.MethodPost, Path: "/runners/{runner}/deregister",
		Summary: "Retire this runner's credential",
		Description: "Runs the runner still holds become lost on the hub's side; runs offered to it and never claimed go back in the queue. " +
			"Every session bound to that runner closes, and the runs still queued in them fail: a session is resumable only on the " +
			"runner that holds it, so a run left in one would be offerable to no runner at all. The binding is never cleared instead — " +
			"the session's state is on that machine. The credential stops working at once. The runner's id and its history stay, so " +
			"registering again with a token issued for that runner brings it back, with new sessions.",
		Security: security, Errors: []int{401, 403, 426},
	}, h.deregister)
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
