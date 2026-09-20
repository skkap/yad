package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// The committed openapi.yaml is what TypeScript hubs generate their types from.
// If it and the Go types disagree, one of them is lying to a hub; this test
// makes the disagreement a failed build instead of a production surprise.
func TestOpenAPIIsCurrent(t *testing.T) {
	gen, err := New(Options{}).OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile("../../protocol/v1/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The committed file starts with a generated-code header the generator adds.
	if _, body, ok := bytes.Cut(committed, []byte("`make generate`.\n")); !ok || !bytes.Equal(body, gen) {
		t.Fatal("protocol/v1/openapi.yaml is stale — run `make generate` and commit the diff")
	}
}

func post(t *testing.T, h http.Handler, path, body string, headers map[string]string) (*http.Response, v1.ErrorEnvelope) {
	t.Helper()
	return call(t, h, http.MethodPost, path, body, headers)
}

func call(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) (*http.Response, v1.ErrorEnvelope) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var env v1.ErrorEnvelope
	if rec.Code >= 400 {
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: error body is not the protocol envelope: %s", path, rec.Body)
		}
	}
	return rec.Result(), env
}

// Every error a hub returns — its own or huma's — has the protocol shape and a
// next action, because a runner has exactly one error decoder.
func TestErrorsHaveTheProtocolShape(t *testing.T) {
	h := New(Options{})
	proto := map[string]string{v1.HeaderProtocol: v1.Version}
	for _, tc := range []struct {
		name, method, path, body string
		headers                  map[string]string
		status                   int
		code                     string
	}{
		{"no credential", "POST", "/v1/runners/r/deregister", `{}`, proto, 401, v1.CodeUnauthorized},
		{"missing protocol header", "POST", "/v1/runners/r/sync", `{}`, nil, 426, v1.CodeUnsupportedProtocol},
		{"another protocol version", "POST", "/v1/runners/r/sync", `{}`, map[string]string{v1.HeaderProtocol: "2"}, 426, v1.CodeUnsupportedProtocol},
		{"malformed body", "POST", "/v1/runs/r/events", `{`, proto, 400, v1.CodeInvalid},
		{"unknown path under the base", "POST", "/v1/runners/r/nope", `{}`, proto, 404, v1.CodeNotFound},
		{"an operation only a newer version has", "POST", "/v1/runners/r/future", `{}`, map[string]string{v1.HeaderProtocol: "2"}, 426, v1.CodeUnsupportedProtocol},
		{"unknown path without the header", "POST", "/v1/runners/r/nope", `{}`, nil, 426, v1.CodeUnsupportedProtocol},
		{"wrong method on an operation", "GET", "/v1/runners/register", ``, proto, 405, v1.CodeInvalid},
		{"outside the base", "POST", "/elsewhere", `{}`, proto, 404, v1.CodeNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, env := call(t, h, tc.method, tc.path, tc.body, tc.headers)
			if res.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", res.StatusCode, tc.status)
			}
			if env.Error.Code != tc.code || env.Error.NextAction == "" || env.Error.Message == "" {
				t.Errorf("error = %+v", env.Error)
			}
			if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("content type %q", ct)
			}
			if tc.status == http.StatusMethodNotAllowed && res.Header.Get("Allow") == "" {
				t.Error("405 without an Allow header")
			}
		})
	}
}

func TestProtocolMountsUnderBasePath(t *testing.T) {
	res, _ := post(t, New(Options{}), "/runners/register", `{}`, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unprefixed path answered %d", res.StatusCode)
	}
}
