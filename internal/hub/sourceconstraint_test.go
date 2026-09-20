package hub

import (
	"net/http"
	"slices"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// One rule with two expressions — the generated schema's oneOf and
// v1.Run.Validate — so one test that fails when they disagree.
//
// The schema half is not "does oneOf appear". Each branch has to carry the
// property it requires: huma checks required inside a loop over a schema's own
// property names, so a branch with Required alone constrains nothing, every
// branch matches every object, and a generated TypeScript union permits
// exactly what the rule forbids. That is not hypothetical — it is what the
// first draft of this constraint did, and the document looked right.
func TestGeneratedSourceCarriesTheOneOf(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  func(*Hub) *huma.OpenAPI
	}{
		{"protocol/v1", func(h *Hub) *huma.OpenAPI { return h.api.OpenAPI() }},
		{"protocol/hubapi", func(h *Hub) *huma.OpenAPI { return h.service.OpenAPI() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := constrainSources(tc.doc(New(Options{}))).Components.Schemas.Map()["Source"]
			if src == nil {
				t.Fatal("the document has no Source schema to constrain")
			}
			if len(src.OneOf) != 2 {
				t.Fatalf("Source has %d oneOf branches, want one per field", len(src.OneOf))
			}
			for _, want := range []string{"git", "path"} {
				i := slices.IndexFunc(src.OneOf, func(b *huma.Schema) bool {
					return len(b.Required) == 1 && b.Required[0] == want
				})
				if i < 0 {
					t.Fatalf("no oneOf branch requires %q", want)
				}
				// The half that a document can look right without.
				if b := src.OneOf[i]; b.Properties[want] == nil {
					t.Errorf("the %q branch requires it without declaring it as a property, so it constrains nothing", want)
				}
			}
		})
	}
}

// And the enforcing half, at the edge a caller actually meets. The schema
// describes the rule for a hub generating types; this is what answers a
// request, and the two must not drift apart.
func TestTheHubEnforcesWhatTheSchemaDescribes(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	tests := []struct {
		name, src, want string
		code            int
	}{
		{name: "both", src: `{"git":{"url":"https://e.com/r.git"},"path":"/tmp/x"}`, code: http.StatusBadRequest, want: "sources[0]: set git or path, not both"},
		{name: "neither", src: `{}`, code: http.StatusBadRequest, want: "sources[0]: set git or path"},
		{name: "path alone is a Source", src: `{"path":"/tmp/x"}`, code: http.StatusCreated},
		{name: "git alone is a Source", src: `{"git":{"url":"https://e.com/r.git"}}`, code: http.StatusCreated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"run-id":"","harness":"claude","model":"sonnet","brief":{"instruction":"hi"},"sources":[` + tc.src + `]}`
			code, env := f.api(t, "POST", "/runs", tok, body, nil)
			if code != tc.code {
				t.Fatalf("status %d (%s), want %d", code, env.Message, tc.code)
			}
			// The message names the rule and which source broke it. huma's
			// own oneOf failure says "matched multiple", which is why the
			// document describes the constraint and Validate enforces it.
			if tc.want != "" && env.Message != tc.want {
				t.Errorf("message = %q, want %q", env.Message, tc.want)
			}
		})
	}
}
