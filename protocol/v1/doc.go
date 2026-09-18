// Package v1 is the runner protocol: the JSON a runner and a hub exchange.
//
// These types are the source of truth. openapi.yaml beside them is generated
// from yad hub's operations over them and committed, and a test fails when the
// two drift — so a field rename here shows up in review as a spec diff that
// every TypeScript hub will feel. Renames and removals are v2, never an edit.
//
// Nothing here is harness-specific. A hub never learns what a rollout file is.
//
// Every list that can be empty is omitempty, and absent means empty. Go
// marshals a nil slice as null, which the published schema — arrays are never
// nullable — would reject; omitting the field is the one encoding both a Go
// runner and a TypeScript hub agree on.
package v1

// Version is the major protocol version, carried in the path and in the
// Yad-Protocol header.
const Version = "1"

// HeaderProtocol names the header every request carries, so a hub can refuse a
// runner speaking a version it does not host before decoding a byte of body.
const HeaderProtocol = "Yad-Protocol"
