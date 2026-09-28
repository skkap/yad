package hub

import (
	"bytes"
	"slices"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// The rule is computed from the document's shape, so its result is pinned
// here: a new enum in something a hub sends becomes extensible in review, not
// by accident. The three are the ones protocol/v1's TestTheV1EnumsAreClosed
// marks as sent towards the runner — gated on protocol_features, decision 0047
// — and every enum a runner sends keeps its enum, which is what a hub
// generated from the document validates strictly.
func TestOnlyWhatAHubSendsIsExtensible(t *testing.T) {
	doc := extendGatedEnums(constrainSources(New(Options{}).api.OpenAPI()))
	var extensible, closed []string
	for name, s := range doc.Components.Schemas.Map() {
		for prop, p := range s.Properties {
			if _, ok := p.Extensions["x-extensible-enum"]; ok {
				extensible = append(extensible, name+"."+prop)
				// Both at once is the form oasdiff still checks on its enum:
				// measured, and the reason extendEnums moves rather than adds.
				if len(p.Enum) > 0 {
					t.Errorf("%s.%s carries enum and x-extensible-enum; oasdiff checks the enum, so a grown value still fails check-breaking", name, prop)
				}
			}
			if len(p.Enum) > 0 {
				closed = append(closed, name+"."+prop)
			}
		}
	}
	slices.Sort(extensible)
	if want := []string{"Control.kind", "Grant.as", "SessionRef.mode"}; !slices.Equal(extensible, want) {
		t.Errorf("extensible enums are %v, want %v. Only an enum a hub sends to a runner grows behind protocol_features (0047, 0058); one a runner sends must stay enum, since a hub validates it", extensible, want)
	}
	// Vacuity: had the walk reached nothing, every enum would be closed and
	// the check above would fail — but a walk that reached everything would
	// leave this list empty.
	for _, want := range []string{"Event.kind", "HeldRun.state", "Result.state", "ClosedSession.reason"} {
		if !slices.Contains(closed, want) {
			t.Errorf("%s lost its enum; a runner sends it, and a hub validates it against the document", want)
		}
	}
}

// The service API has no feature advertisement, so a value it grows reaches a
// caller unannounced: that is the enum growth check-breaking exists to catch,
// and it must stay an enum.
func TestTheServiceAPIHasNoExtensibleEnum(t *testing.T) {
	b, err := ServiceOpenAPIYAML()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("x-extensible-enum")) {
		t.Error("protocol/hubapi's document carries x-extensible-enum; a service has no way to advertise a feature, so none of its enums may grow unchecked")
	}
	if !bytes.Contains(b, []byte("enum:")) {
		t.Error("protocol/hubapi's document carries no enum at all, so the check above proves nothing")
	}
}

// Nothing here reaches a schema a request body names, so a hub whose
// registry this was applied to validates what it did before. Held directly,
// since the rule's safety claim rests on it.
func TestExtendingEnumsLeavesRequestsAlone(t *testing.T) {
	doc := extendGatedEnums(New(Options{}).api.OpenAPI())
	requests := map[string]bool{}
	for _, p := range doc.Paths {
		if p.Post != nil && p.Post.RequestBody != nil {
			for _, mt := range p.Post.RequestBody.Content {
				reach(doc, mt.Schema, requests)
			}
		}
	}
	if len(requests) == 0 {
		t.Fatal("reached no request schema, so this proves nothing")
	}
	for name := range requests {
		var walk func(*huma.Schema)
		walk = func(s *huma.Schema) {
			if s == nil || s.Ref != "" {
				return
			}
			if _, ok := s.Extensions["x-extensible-enum"]; ok {
				t.Errorf("%s, which a request body reaches, carries x-extensible-enum", name)
			}
			for _, c := range children(s) {
				walk(c)
			}
		}
		walk(doc.Components.Schemas.Map()[name])
	}
}
