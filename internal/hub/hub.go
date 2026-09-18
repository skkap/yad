// Package hub is `yad hub`: the standalone hub, and the reference
// implementation of the server half of the v1 protocol.
//
// It serves three roles, which is why it lives in this repository: a hub for
// services that would rather not embed the protocol (decision 0003), the fake
// the runner's own tests run against, and the source of protocol/v1/openapi.yaml
// — the operations below are declared over the protocol/v1 types, and the
// document is generated from them (decision 0017).
//
// The operations are declared and documented; their behaviour arrives in epic E2
// (Zumino yad/dev). Until then each answers not_implemented.
package hub

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	v1 "github.com/skkap/yad/protocol/v1"
)

// BasePath is where `yad hub` mounts the protocol. Other hubs choose their own
// base; every path in the protocol is relative to it.
const BasePath = "/v1"

// Hub is the protocol server.
type Hub struct {
	api huma.API
	mux *http.ServeMux
}

// New builds a hub with every v1 operation registered.
func New() *Hub {
	inner := http.NewServeMux()
	api := humago.New(inner, Config())
	register(api)

	outer := http.NewServeMux()
	outer.Handle(BasePath+"/", http.StripPrefix(BasePath, inner))
	// Anything outside the base is a wrong connection URL. Say so in the
	// protocol's own shape, not the mux's plain-text 404.
	outer.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, Fail(http.StatusNotFound, v1.CodeNotFound, "no protocol at "+r.URL.Path,
			"the connection URL must end in the hub's base, e.g. https://host"+BasePath))
	})
	return &Hub{api: api, mux: outer}
}

// ServeHTTP serves the protocol under BasePath.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// OpenAPI returns the generated document as YAML.
func (h *Hub) OpenAPI() ([]byte, error) { return h.api.OpenAPI().YAML() }

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
// host before decoding the body.
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

func register(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "register", Method: http.MethodPost, Path: "/runners/register",
		Summary:     "Exchange a registration token for a runner credential",
		Description: "Called once by `yad connect`, with the one-time registration token as the bearer. The token is dead afterwards.",
		Security:    security, Errors: []int{400, 401, 409, 426},
	}, func(ctx context.Context, in *registerInput) (*registerOutput, error) {
		return nil, notYet("register")
	})

	huma.Register(api, huma.Operation{
		OperationID: "sync", Method: http.MethodPost, Path: "/runners/{runner}/sync",
		Summary: "Heartbeat, lease renewal, health and the ask for work, in one call",
		Description: "Every run listed is claimed or has its lease renewed. A run offered in the previous response and not listed " +
			"here was never received and will be offered again.",
		Security: security, Errors: []int{400, 401, 403, 426},
	}, func(ctx context.Context, in *syncInput) (*syncOutput, error) {
		return nil, notYet("sync")
	})

	huma.Register(api, huma.Operation{
		OperationID: "appendEvents", Method: http.MethodPost, Path: "/runs/{run}/events",
		Summary:     "Append a batch of run events",
		Description: "Idempotent by (run, seq). The response's acked_through is authoritative; the runner resends everything after it.",
		Security:    security, Errors: []int{400, 401, 404},
	}, func(ctx context.Context, in *eventsInput) (*eventsOutput, error) {
		return nil, notYet("appendEvents")
	})

	huma.Register(api, huma.Operation{
		OperationID: "submitResult", Method: http.MethodPost, Path: "/runs/{run}/result",
		Summary:     "Report a run's terminal state",
		Description: "Retried from the runner's outbox until acknowledged. 409 means the hub already holds a different terminal state, which wins.",
		Security:    security, Errors: []int{400, 401, 404, 409},
	}, func(ctx context.Context, in *resultInput) (*ackOutput, error) {
		return nil, notYet("submitResult")
	})

	huma.Register(api, huma.Operation{
		OperationID: "deregister", Method: http.MethodPost, Path: "/runners/{runner}/deregister",
		Summary:     "Retire this runner's credential",
		Description: "Runs the runner still holds become lost on the hub's side.",
		Security:    security, Errors: []int{401},
	}, func(ctx context.Context, in *deregisterInput) (*ackOutput, error) {
		return nil, notYet("deregister")
	})
}

func notYet(op string) error {
	return Fail(http.StatusNotImplemented, v1.CodeNotImplemented,
		"yad hub does not implement "+op+" yet",
		"the hub's behaviour arrives in epic E2 (Zumino yad/dev) — see ARCHITECTURE.md §9")
}

func writeError(w http.ResponseWriter, e *ErrorResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(e)
}
