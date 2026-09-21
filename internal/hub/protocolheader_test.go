package hub

import (
	"net/http"
	"regexp"
	"slices"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"
)

// The Yad-Protocol header is enforced by protocolRoutes, outside huma, and
// declared to huma separately so it reaches the documents. Nothing ties the
// two together, and once they drifted for the whole of v1: the header was
// enforced on every call and declared on none (DEV-93). So enforcement is
// measured here rather than assumed — every operation of each document is
// called without the header — and the document must declare exactly where the
// hub refuses: a required Yad-Protocol of 1, and the 426 the refusal is.
func TestProtocolHeaderIsDeclaredWhereEnforced(t *testing.T) {
	f := newFixture(t)
	admin := f.admin(t, "probe")
	docs := New(Options{})
	for _, d := range []struct {
		name, base, bearer string
		doc                *huma.OpenAPI
		// enforced is how many operations the hub refuses without the
		// header, so a probe that stopped reaching the check — a new guard
		// in front of it — fails here instead of passing vacuously.
		enforced int
	}{
		{"protocol/v1", BasePath, "", docs.api.OpenAPI(), 5},
		{"protocol/hubapi", hubapi.BasePath, admin, docs.service.OpenAPI(), 0},
	} {
		t.Run(d.name, func(t *testing.T) {
			enforced := 0
			for path, item := range d.doc.Paths {
				for method, op := range operations(item) {
					res, env := call(t, f.hub, method, d.base+pathParam.ReplaceAllString(path, "x"), `{}`, bearerHeader(d.bearer))
					refused := res.StatusCode == http.StatusUpgradeRequired && env.Error.Code == v1.CodeUnsupportedProtocol
					declared, has426 := declaresProtocolHeader(op), op.Responses["426"] != nil
					switch {
					case refused && !declared:
						t.Errorf("%s %s refuses a request without %s but does not declare it", method, path, v1.HeaderProtocol)
					case refused && !has426:
						t.Errorf("%s %s refuses with 426 but does not list it", method, path)
					case !refused && declared:
						t.Errorf("%s %s declares %s but answers %d without it", method, path, v1.HeaderProtocol, res.StatusCode)
					}
					if refused {
						enforced++
					}
				}
			}
			if enforced != d.enforced {
				t.Errorf("%d operations refuse a request without %s, want %d", enforced, v1.HeaderProtocol, d.enforced)
			}
		})
	}
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

func operations(p *huma.PathItem) map[string]*huma.Operation {
	ops := map[string]*huma.Operation{}
	for method, op := range map[string]*huma.Operation{
		http.MethodGet: p.Get, http.MethodPut: p.Put, http.MethodPost: p.Post, http.MethodDelete: p.Delete,
		http.MethodOptions: p.Options, http.MethodHead: p.Head, http.MethodPatch: p.Patch, http.MethodTrace: p.Trace,
	} {
		if op != nil {
			ops[method] = op
		}
	}
	return ops
}

// declaresProtocolHeader is true only for the declaration a generated client
// can act on: required, and naming the one value this version accepts.
func declaresProtocolHeader(op *huma.Operation) bool {
	for _, p := range op.Parameters {
		if p.In == "header" && http.CanonicalHeaderKey(p.Name) == v1.HeaderProtocol {
			return p.Required && p.Schema != nil && slices.Equal(p.Schema.Enum, []any{v1.Version})
		}
	}
	return false
}

func bearerHeader(secret string) map[string]string {
	if secret == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + secret}
}
