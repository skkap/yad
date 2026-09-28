package hub

import (
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// An enum a hub sends to a runner is written in protocol/v1's document as
// x-extensible-enum rather than enum — decision 0058.
//
// Every v1 enum is closed, and grows only behind a feature the receiving side
// advertised (0047). For what a runner sends that is invisible to a
// breaking-change check and needs nothing here: oasdiff rates a request enum
// that gained a value as information, never as breaking. For what a hub sends it is the
// opposite. A new control kind is a response enum grown, which oasdiff rates
// an error whatever the threshold, because it cannot see that a hub sends the
// value only to a runner advertising it in protocol_features. So the repo's own
// way of growing the protocol would fail its own gate on every release that
// used it.
//
// x-extensible-enum is the one form the pinned oasdiff (v1.32.1) reads as a set
// meant to grow, and only *in place of* enum: a schema carrying both is still
// checked on its enum, and still fails. That was measured, not read.
//
// The rule is by direction and document, not a list of names, because the
// direction is what makes the growth safe:
//
//   - protocol/v1 only. The service API has no feature advertisement at all,
//     so a value a service did not know about reaches it unannounced — the
//     enum growth that really does break a caller, which the check must keep
//     catching.
//   - Only a schema reachable from a response body and from no request body.
//     A hub generated from the document validates request enums strictly, and
//     0047 relies on it; loosening one of those would widen what a hub accepts
//     to buy nothing, since oasdiff never rated it breaking. A schema on both sides
//     keeps its enum, so its growth still fails the check — loud, which is the
//     right way to be wrong.
//
// TestOnlyWhatAHubSendsIsExtensible pins the result — control kind, grant
// delivery, session mode, the three TestTheV1EnumsAreClosed marks as sent
// towards the runner — so a new one is seen in review rather than by accident.
//
// huma validates request bodies only, and this touches no schema a request
// reaches, so unlike constrainSources it cannot change how a hub answers
// whichever registry it is given. It is still only given the generator's.
func extendGatedEnums(doc *huma.OpenAPI) *huma.OpenAPI {
	requests, responses := map[string]bool{}, map[string]bool{}
	for _, p := range doc.Paths {
		for _, op := range []*huma.Operation{p.Get, p.Put, p.Post, p.Delete, p.Options, p.Head, p.Patch, p.Trace} {
			if op == nil {
				continue
			}
			if op.RequestBody != nil {
				for _, mt := range op.RequestBody.Content {
					reach(doc, mt.Schema, requests)
				}
			}
			for _, r := range op.Responses {
				for _, mt := range r.Content {
					reach(doc, mt.Schema, responses)
				}
			}
		}
	}
	for name := range responses {
		if !requests[name] {
			extendEnums(doc.Components.Schemas.Map()[name])
		}
	}
	return doc
}

// reach adds to seen every component schema s names, directly or through
// another.
func reach(doc *huma.OpenAPI, s *huma.Schema, seen map[string]bool) {
	if s == nil {
		return
	}
	if name, ok := strings.CutPrefix(s.Ref, "#/components/schemas/"); ok {
		if !seen[name] {
			seen[name] = true
			reach(doc, doc.Components.Schemas.Map()[name], seen)
		}
		return
	}
	for _, c := range children(s) {
		reach(doc, c, seen)
	}
}

// extendEnums rewrites every enum inside one component, stopping at a
// reference: the component it names is decided on its own reachability.
func extendEnums(s *huma.Schema) {
	if s == nil || s.Ref != "" {
		return
	}
	if len(s.Enum) > 0 {
		if s.Extensions == nil {
			s.Extensions = map[string]any{}
		}
		s.Extensions["x-extensible-enum"] = s.Enum
		s.Enum = nil
	}
	for _, c := range children(s) {
		extendEnums(c)
	}
}

func children(s *huma.Schema) []*huma.Schema {
	out := []*huma.Schema{s.Items, s.Not}
	for _, p := range s.Properties {
		out = append(out, p)
	}
	out = append(out, s.OneOf...)
	out = append(out, s.AnyOf...)
	out = append(out, s.AllOf...)
	if ap, ok := s.AdditionalProperties.(*huma.Schema); ok {
		out = append(out, ap)
	}
	return out
}
