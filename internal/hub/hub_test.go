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
	gen, err := New().OpenAPI()
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
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
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
	h := New()
	proto := map[string]string{v1.HeaderProtocol: v1.Version}
	for _, tc := range []struct {
		name, path, body string
		headers          map[string]string
		status           int
		code             string
	}{
		{"not built yet", "/v1/runners/register", `{"capabilities":{"runner_id":"r","name":"n","yad_version":"dev","os":"linux","arch":"amd64","harnesses":[],"capacity":{"total":1},"observed_at":"2026-09-18T00:00:00Z"}}`, proto, 501, v1.CodeNotImplemented},
		{"missing protocol header", "/v1/runners/r/sync", `{}`, nil, 422, v1.CodeInvalid},
		{"malformed body", "/v1/runs/r/events", `{`, proto, 400, v1.CodeInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, env := post(t, h, tc.path, tc.body, tc.headers)
			if res.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", res.StatusCode, tc.status)
			}
			if env.Error.Code != tc.code || env.Error.NextAction == "" || env.Error.Message == "" {
				t.Errorf("error = %+v", env.Error)
			}
			if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("content type %q", ct)
			}
		})
	}
}

func TestProtocolMountsUnderBasePath(t *testing.T) {
	res, _ := post(t, New(), "/runners/register", `{}`, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unprefixed path answered %d", res.StatusCode)
	}
}
