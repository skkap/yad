package v1

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Every enum the document declares is closed for all of v1 (decision 0047,
// HUB.md §2 and §11). A hub validates against the document and refuses a value
// outside the set, and a runner drops an events batch or a result refused as
// invalid — so a value added here and sent to a hub built from an older
// document is not a new feature, it is lost work.
//
// This pins each set as v1 shipped it. A test that fails on a new value is the
// point: whoever adds one reads here that it may go only to the other side
// once it has advertised the feature adding it — a hub in hub_features, a
// runner in protocol_features — and that a yad runner keeps no hub_features
// today (Connect drops them), so a value it sends needs that first.
func TestTheV1EnumsAreClosed(t *testing.T) {
	for _, tc := range []struct {
		v     any
		field string
		// towards is the side that must have advertised a new value's
		// feature before it may be sent.
		towards string
		v1      []string
	}{
		{Event{}, "Kind", "hub", []string{"text", "thinking", "tool_call", "tool_result", "status", "usage", "error"}},
		{HeldRun{}, "State", "hub", []string{"claimed", "preparing", "running", "waiting"}},
		{Result{}, "State", "hub", []string{"succeeded", "failed", "cancelled", "timed_out", "lost"}},
		{ClosedSession{}, "Reason", "hub", []string{"closed", "closed_by_owner", "expired", "disk_pressure"}},
		{AccountReport{}, "State", "hub", []string{"free", "limited", "needs_login"}},
		{HarnessReport{}, "Kind", "hub", []string{"first-class", "recognised"}},
		// Came with its field (DEV-50): a hub built before it has no
		// models_source to validate.
		{HarnessReport{}, "ModelsSource", "hub", []string{"harness", "catalog"}},
		// The four login kinds came after v1 shipped, gated on the "login"
		// feature a runner advertises (decision 0055); remove_account after
		// them, gated on "accounts" (decision 0057).
		{Control{}, "Kind", "runner", []string{"cancel", "interrupt", "steer", "close_session", "drain", "report_capabilities", "update", "start_login", "login_code", "login_token", "cancel_login", "remove_account"}},
		// New with hub login, and sent only to a hub that asked for a login.
		{LoginReport{}, "Method", "hub", []string{"link", "token"}},
		{LoginReport{}, "State", "hub", []string{"starting", "waiting", "checking", "succeeded", "failed", "expired", "cancelled"}},
		{SessionRef{}, "Mode", "runner", []string{"per_run", "live"}},
		{Grant{}, "As", "runner", []string{"env", "file"}},
	} {
		f, ok := reflect.TypeOf(tc.v).FieldByName(tc.field)
		if !ok {
			t.Fatalf("%T has no field %s", tc.v, tc.field)
		}
		got := strings.Split(f.Tag.Get("enum"), ",")
		if slices.Equal(got, tc.v1) {
			continue
		}
		features := "hub_features"
		if tc.towards == "runner" {
			features = "protocol_features"
		}
		t.Errorf("%T.%s is %v, and v1 shipped it as %v. v1's enums are closed (decision 0047): a new value goes to a %s only once it has advertised, in %s, the feature adding it — gate it, then add it here",
			tc.v, tc.field, got, tc.v1, tc.towards, features)
	}
}

// Every enum a runner sends is covered above: an enum-tagged field of a type
// the runner sends that the list above does not name would be a set nobody
// promised to keep closed.
func TestEveryEnumIsPinned(t *testing.T) {
	pinned := map[string]bool{
		"Event.Kind": true, "HeldRun.State": true, "Result.State": true, "ClosedSession.Reason": true,
		"AccountReport.State": true, "HarnessReport.Kind": true, "HarnessReport.ModelsSource": true, "Control.Kind": true, "SessionRef.Mode": true, "Grant.As": true,
		"LoginReport.Method": true, "LoginReport.State": true,
	}
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(ty reflect.Type) {
		for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Map {
			ty = ty.Elem()
		}
		if ty.Kind() != reflect.Struct || ty.PkgPath() != reflect.TypeFor[Run]().PkgPath() || seen[ty] {
			return
		}
		seen[ty] = true
		for i := range ty.NumField() {
			f := ty.Field(i)
			if f.Tag.Get("enum") != "" && !pinned[ty.Name()+"."+f.Name] {
				t.Errorf("%s.%s is an enum of the document that TestTheV1EnumsAreClosed does not pin", ty.Name(), f.Name)
			}
			walk(f.Type)
		}
	}
	for _, root := range []any{RegisterRequest{}, RegisterResponse{}, SyncRequest{}, SyncResponse{}, EventBatch{}, Result{}} {
		walk(reflect.TypeOf(root))
	}
}
