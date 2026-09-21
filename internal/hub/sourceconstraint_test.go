package hub

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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

// Generating the document must not change how a hub answers.
//
// constrainSources writes into a schema registry, and in huma the document a
// hub hands out *is* the registry it validates bodies against. While
// generating was a method, a hub that generated its own document began
// enforcing the oneOf: the same request that had been 400 with "set git or
// path, not both" came back 422 with huma's "matched multiple". Measured, on
// one hub, with a single ServiceOpenAPI() call between two identical requests.
//
// The measurement that missed it was taken on a fresh hub — the configuration
// that cannot show the difference — rather than on one that had generated the
// document, which is the configuration that varies.
//
// This cannot be made red against the code as it stands, and that is the fix
// rather than a gap in the test: generating is no longer a method, so a hub
// has no way to generate its own document and the defect is not expressible.
// What this holds is the behaviour through the route that does exist.
func TestGeneratingTheDocumentDoesNotChangeHowAHubAnswers(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	both := `{"harness":"claude","model":"sonnet","brief":{"instruction":"hi"},"sources":[{"git":{"url":"https://e.com/r.git"},"path":"/tmp/x"}]}`
	for _, when := range []string{"before generating", "after generating"} {
		code, env := f.api(t, "POST", "/runs", tok, both, nil)
		if code != http.StatusBadRequest || env.Message != "sources[0]: set git or path, not both" {
			t.Errorf("%s: HTTP %d %q; want 400 and the message that names the rule", when, code, env.Message)
		}
		if when == "before generating" {
			for _, gen := range []func() ([]byte, error){OpenAPIYAML, ServiceOpenAPIYAML} {
				if _, err := gen(); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// And the invariant itself, which is what catches a future method: whatever
// has been generated, the schemas a serving hub validates against carry no
// oneOf. This one does go red against a generator that mutates the hub it is
// given — it is the assertion, not the symptom.
func TestAServingHubsRegistryNeverCarriesTheConstraint(t *testing.T) {
	h := New(Options{})
	if _, err := ServiceOpenAPIYAML(); err != nil {
		t.Fatal(err)
	}
	// Deliberately the same shape a method would have had.
	constrained := constrainSources(New(Options{}).service.OpenAPI())
	if constrained.Components.Schemas.Map()["Source"].OneOf == nil {
		t.Fatal("constrainSources added nothing, so this test proves nothing")
	}
	for name, doc := range map[string]*huma.OpenAPI{"protocol": h.api.OpenAPI(), "service": h.service.OpenAPI()} {
		if src := doc.Components.Schemas.Map()["Source"]; src != nil && src.OneOf != nil {
			t.Errorf("%s: a live hub's Source schema carries the oneOf, so this hub validates against it", name)
		}
	}
}

// The guard that can fail, which the other two cannot.
//
// What keeps a serving hub's registry unconstrained is that constrainSources
// is only ever handed a document from a hub built for the purpose and thrown
// away. Nothing in the type system says so: hand it a live hub's document and
// that hub begins validating against the oneOf, answering 422 where
// Run.Validate should answer 400. That is the defect this branch fixed, and
// removing the methods removed the obvious way to reintroduce it — not every
// way.
//
// So the enforcement is: no shipped file calls constrainSources except the two
// generators, which construct their own hub in the same expression. A call
// anywhere else fails here, naming itself. Matching on source text is a
// backstop rather than a proof — a document reached through a variable several
// hops away is past what reading can see — but it catches the line someone
// would actually write.
func TestConstrainSourcesIsReachedOnlyFromTheGenerators(t *testing.T) {
	const root = "../.."
	allowed := map[string]bool{
		"constrainSources(New(Options{}).api.OpenAPI())":     true,
		"constrainSources(New(Options{}).service.OpenAPI())": true,
	}
	// One level of nesting is enough for the calls that exist, and the
	// declaration is excluded by requiring no "func " before the name.
	call := regexp.MustCompile(`(?:func )?constrainSources\((?:[^()]|\([^()]*\))*\)`)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata"):
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range call.FindAll(raw, -1) {
			if strings.HasPrefix(string(m), "func ") {
				continue // the declaration itself
			}
			if !allowed[string(m)] {
				t.Errorf("%s calls %s; constrainSources may only be given a document from a hub built for it and discarded, or that hub starts validating against the oneOf", path, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A rename or a rewrite that stops either generator matching would make
	// this vacuous, so the allowed forms must actually be present.
	found := 0
	for _, f := range []string{"hub.go"} {
		raw, err := os.ReadFile(filepath.Join("..", "hub", f))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range call.FindAll(raw, -1) {
			if allowed[string(m)] {
				found++
			}
		}
	}
	if found != len(allowed) {
		t.Errorf("found %d of the %d expected generator calls; this test is passing because it matches nothing", found, len(allowed))
	}
}
