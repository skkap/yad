package hub

import "github.com/danielgtaylor/huma/v2"

// A Source is exactly one of git or path. v1.Run.Validate enforces it; the
// generated schema describes it, so a hub generating types from the document
// gets the rule its Go equivalent gets for free.
//
// Why it is added here rather than by a Schema method on v1.Source, which
// would be the obvious way: huma's SchemaProvider lives in huma, and
// protocol/v1 has no third-party dependency at all today. That package is the
// wire contract, imported by every other package here and by anyone embedding
// this protocol in Go, and a server framework is not something it should carry
// to describe a constraint.
//
// And it would not consolidate anything. v1.Run.Validate is called in four
// places outside this hub's HTTP edge — internal/runner's connect, sync and
// executor, and internal/workdir — because a runner validates every run a hub
// sends it. Validate stays under either design, so a huma schema that enforced
// the rule too would be a third expression of it rather than a second.
//
// The consequence, deliberately chosen: huma validates request bodies against
// the schemas in its registry, not against this document, so it does not
// enforce the oneOf. A caller sending both therefore gets what Validate says —
// 400 with "sources[0]: set git or path, not both" — rather than huma's
// "expected value to match exactly one schema but matched multiple" and a 422.
// The rule reaches generated types; the message a person reads stays the one
// that names the rule.
//
// The branches carry Properties as well as Required, and that is not
// decoration. huma checks required inside a loop over a schema's own property
// names, so a branch declaring Required alone constrains nothing and every
// branch matches every object — which an earlier draft of this shipped as a
// oneOf that rejected every valid source. A generated union built from such
// branches would permit exactly what it is meant to forbid.
// TestGeneratedSourceCarriesTheOneOf holds both halves.
func constrainSources(doc *huma.OpenAPI) *huma.OpenAPI {
	for _, s := range doc.Components.Schemas.Map() {
		addSourceOneOf(s, map[*huma.Schema]bool{})
	}
	return doc
}

// addSourceOneOf walks a schema and constrains every Source-shaped object it
// finds. Source is inlined rather than named — only GitSource gets a component
// of its own — so there is no single place to reach for, and it appears in
// both documents: Run.sources in the protocol, SubmitRequest.sources in the
// service API.
//
// Shape is the whole test: an object whose properties are exactly git and
// path. Nothing else in either document has those two and nothing else should
// gain the constraint, which is why this is one named rule rather than a
// general pass over the document.
func addSourceOneOf(s *huma.Schema, seen map[*huma.Schema]bool) {
	if s == nil || seen[s] {
		return
	}
	seen[s] = true
	if len(s.Properties) == 2 && s.Properties["git"] != nil && s.Properties["path"] != nil && s.OneOf == nil {
		s.OneOf = []*huma.Schema{
			{Type: "object", Properties: map[string]*huma.Schema{"git": s.Properties["git"]}, Required: []string{"git"}},
			{Type: "object", Properties: map[string]*huma.Schema{"path": s.Properties["path"]}, Required: []string{"path"}},
		}
	}
	for _, p := range s.Properties {
		addSourceOneOf(p, seen)
	}
	addSourceOneOf(s.Items, seen)
	for _, b := range s.OneOf {
		addSourceOneOf(b, seen)
	}
}
