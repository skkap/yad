package hub

import (
	"net/http"
	"strings"
	"testing"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// Adding and removing a runner's accounts through yad hub (decision 0057).

// managing is the first sync of a runner whose owner lets this hub add and
// remove accounts, naming claude's accounts in its health.
func managing(id string, labels ...string) v1.SyncRequest {
	r := first(id, 1)
	r.Fingerprint = "fp-accounts-" + id
	r.Capabilities.ProtocolFeatures = append(capability.Features(r.Capabilities.Harnesses), capability.FeatureAccounts)
	for _, l := range labels {
		r.Capabilities.Harnesses[0].Accounts = append(r.Capabilities.Harnesses[0].Accounts, v1.AccountReport{Label: l, State: v1.AccountFree})
	}
	return withAccounts(r, labels...)
}

func withAccounts(r v1.SyncRequest, labels ...string) v1.SyncRequest {
	hh := v1.HarnessHealth{ID: "claude", Ready: true}
	for _, l := range labels {
		hh.Accounts = append(hh.Accounts, v1.AccountReport{Label: l, State: v1.AccountFree})
	}
	r.Health.Harnesses = []v1.HarnessHealth{hh}
	return r
}

// later is a sync after the first, under the same fingerprint.
func later(first v1.SyncRequest) v1.SyncRequest {
	first.Capabilities = nil
	return first
}

// onlyCommand is the one yad command in a next action.
func onlyCommand(t *testing.T, msg string) string {
	t.Helper()
	cmds := shellwordtest.Commands(msg, "yad ")
	if len(cmds) != 1 {
		t.Fatalf("want one command in %q", msg)
	}
	return cmds[0]
}

func removals(res v1.SyncResponse) []v1.Control {
	var out []v1.Control
	for _, c := range res.Controls {
		if c.Kind == v1.ControlRemoveAccount {
			out = append(out, c)
		}
	}
	return out
}

// An add is a login carrying add: refused without a label, and by a runner
// that does not advertise accounts; stored, and delivered with add to one
// that does.
func TestAnAddThroughYadHub(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	plain := f.register(t, "r0")
	f.mustSync(t, "r0", plain, first("r0", 1))
	cred := f.register(t, "r1")
	f.mustSync(t, "r1", cred, managing("r1", "work"))

	for _, c := range []struct {
		name, runner string
		req          hubapi.LoginRequest
		code         int
		says         string
	}{
		{"no label", "r1", hubapi.LoginRequest{Harness: "claude", Add: true}, http.StatusBadRequest, "names none"},
		{"a runner without accounts", "r0", hubapi.LoginRequest{Harness: "claude", Account: "second", Add: true}, http.StatusConflict, "manage_accounts = false"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, e := f.api(t, "POST", "/runners/"+c.runner+"/logins", tok, c.req, nil)
			if code != c.code || !strings.Contains(e.Message, c.says) {
				t.Fatalf("%d %+v, want %d saying %q", code, e, c.code, c.says)
			}
			if c.code == http.StatusConflict {
				shellwordtest.Check(t, onlyCommand(t, e.NextAction),
					"yad", "--profile", "<runner profile>", "account", "add", "claude", "second")
			}
		})
	}

	var view hubapi.Login
	if code, e := f.api(t, "POST", "/runners/r1/logins", tok, hubapi.LoginRequest{LoginID: "lg1", Harness: "claude", Account: "second", Add: true}, &view); code != http.StatusCreated || !view.Add {
		t.Fatalf("start: %d %+v %+v", code, e, view)
	}
	cs := loginControls(f.mustSync(t, "r1", cred, later(managing("r1", "work"))))
	if len(cs) != 1 || cs[0].Kind != v1.ControlStartLogin || !cs[0].Add || cs[0].Account != "second" {
		t.Fatalf("controls %+v, want start_login with add", cs)
	}
}

// An add not yet sent to a runner that has since stopped advertising accounts
// — its owner turned manage_accounts off and restarted it — ends failed at
// once, never sent, saying what to do; while the runner's new document has
// not arrived it is held back, as every gated control is.
func TestAnAddARunnerNoLongerTakesEnds(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	f.mustSync(t, "r1", cred, managing("r1"))
	if code, e := f.api(t, "POST", "/runners/r1/logins", tok, hubapi.LoginRequest{LoginID: "lg1", Harness: "claude", Account: "second", Add: true}, nil); code != http.StatusCreated {
		t.Fatalf("start: %d %+v", code, e)
	}
	// The fingerprint moved; the document that says why has not come.
	moved := req("r1", 1)
	moved.Fingerprint = "fp-off"
	if cs := loginControls(f.mustSync(t, "r1", cred, moved)); len(cs) != 0 {
		t.Fatalf("sent %+v before the new document arrived", cs)
	}
	if l := f.login(t, tok, "lg1"); l.State != hubapi.LoginRequested {
		t.Fatalf("ended %s before the new document arrived", l.State)
	}
	off := first("r1", 1)
	off.Fingerprint = "fp-off"
	if cs := loginControls(f.mustSync(t, "r1", cred, off)); len(cs) != 0 {
		t.Fatalf("sent %+v to a runner that does not advertise accounts", cs)
	}
	l := f.login(t, tok, "lg1")
	if l.State != hubapi.LoginState(v1.LoginFailed) || !strings.Contains(l.Error, "does not let this hub add accounts") {
		t.Fatalf("the add is %s: %q", l.State, l.Error)
	}
	shellwordtest.Check(t, onlyCommand(t, l.Error), "yad", "--profile", "<runner profile>", "account", "add", "claude", "second")
}

// A removal: refused by a runner that does not advertise accounts; otherwise
// remove_account in every answer until the runner's health leaves the account
// out — and not before the runner's document has said it takes one.
func TestARemovalThroughYadHub(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	plain := f.register(t, "r0")
	f.mustSync(t, "r0", plain, first("r0", 1))
	cred := f.register(t, "r1")
	on := managing("r1", "work", "spare")
	f.mustSync(t, "r1", cred, on)

	code, e := f.api(t, "POST", "/runners/r0/accounts/claude/work/remove", tok, nil, nil)
	if code != http.StatusConflict {
		t.Fatalf("a runner without accounts: %d %+v", code, e)
	}
	shellwordtest.Check(t, onlyCommand(t, e.NextAction), "yad", "--profile", "<runner profile>", "account", "remove", "claude", "work")
	for _, path := range []string{"/runners/nope/accounts/claude/work/remove", "/runners/r1/accounts/claude/Not%20A%20Label/remove"} {
		if code, _ := f.api(t, "POST", path, tok, nil, nil); code != http.StatusNotFound && code != http.StatusBadRequest {
			t.Errorf("%s: %d", path, code)
		}
	}

	var view hubapi.Account
	if code, e := f.api(t, "POST", "/runners/r1/accounts/claude/work/remove", tok, nil, &view); code != http.StatusOK || !view.Listed || view.RemoveRequestedAt == nil {
		t.Fatalf("remove: %d %+v %+v", code, e, view)
	}
	for range 2 {
		cs := removals(f.mustSync(t, "r1", cred, later(on)))
		if len(cs) != 1 || cs[0].Harness != "claude" || cs[0].Account != "work" {
			t.Fatalf("controls %+v, want remove_account for work", cs)
		}
	}
	// Either report alone still listing it is not the account gone.
	capped := later(withAccounts(on, "spare"))
	if cs := removals(f.mustSync(t, "r1", cred, capped)); len(cs) != 1 {
		t.Fatalf("health left it out and the document lists it: controls %+v, want the removal still sent", cs)
	}
	// The runner acted: its new document and its health both leave it out.
	acted := managing("r1", "spare")
	acted.Fingerprint = "fp-acted"
	f.mustSync(t, "r1", cred, acted)
	var gone hubapi.Account
	f.api(t, "GET", "/runners/r1/accounts/claude/work", tok, nil, &gone)
	if gone.Listed || gone.State != "" || gone.RemoveRequestedAt != nil {
		t.Fatalf("after health left it out: %+v", gone)
	}
	if cs := removals(f.mustSync(t, "r1", cred, later(on))); len(cs) != 0 {
		t.Fatalf("still sent once health left it out: %+v", cs)
	}
}

// A health that names no harness — a runner that could not read its accounts
// this time — does not end a removal; one naming the harness without the
// account does. And a runner that stops advertising accounts ends it, since it
// would ignore the control.
func TestARemovalEndsOnlyOnAbsenceOrTheFeatureGone(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	on := managing("r1", "work")
	f.mustSync(t, "r1", cred, on)
	if code, e := f.api(t, "POST", "/runners/r1/accounts/claude/work/remove", tok, nil, nil); code != http.StatusOK {
		t.Fatalf("remove: %d %+v", code, e)
	}
	blind := later(on)
	blind.Health.Harnesses = nil
	for range 2 {
		if cs := removals(f.mustSync(t, "r1", cred, blind)); len(cs) != 1 {
			t.Fatalf("a health naming no harness: controls %+v, want the removal still sent", cs)
		}
	}
	off := first("r1", 1)
	off.Fingerprint = "fp-off"
	f.mustSync(t, "r1", cred, withAccounts(off, "work"))
	var view hubapi.Account
	f.api(t, "GET", "/runners/r1/accounts/claude/work", tok, nil, &view)
	if view.RemoveRequestedAt != nil || !view.Listed {
		t.Fatalf("a runner that stopped advertising accounts: %+v, want the removal ended and the account still listed", view)
	}
}

// Removing a harness's last account on a machine with no default login makes
// the harness one the runner cannot drive, and health stops naming it. The
// runner's document, rebuilt at once, still names the harness — without the
// account — and that ends the removal, rather than it going out for ever and
// taking away the same label when it is added again.
func TestARemovalEndsWhenTheHarnessLeavesHealth(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	on := managing("r1", "work")
	f.mustSync(t, "r1", cred, on)
	if code, e := f.api(t, "POST", "/runners/r1/accounts/claude/work/remove", tok, nil, nil); code != http.StatusOK {
		t.Fatalf("remove: %d %+v", code, e)
	}
	if cs := removals(f.mustSync(t, "r1", cred, later(on))); len(cs) != 1 {
		t.Fatalf("controls %+v, want the removal", cs)
	}
	// The runner acted: its harness left health, and its new document
	// names claude with no account.
	after := managing("r1")
	after.Fingerprint = "fp-after"
	after.Health.Harnesses = nil
	if cs := removals(f.mustSync(t, "r1", cred, after)); len(cs) != 0 {
		t.Fatalf("still sent once the document left the account out: %+v", cs)
	}
	var v hubapi.Account
	f.api(t, "GET", "/runners/r1/accounts/claude/work", tok, nil, &v)
	if v.RemoveRequestedAt != nil {
		t.Fatalf("the removal stands: %+v", v)
	}
}

// Adding an account again is the owner's newer word: a removal still waiting
// for it ends, rather than going out once the account is back.
func TestAnAddEndsAWaitingRemovalOfItsLabel(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	on := managing("r1", "work")
	f.mustSync(t, "r1", cred, on)
	if code, e := f.api(t, "POST", "/runners/r1/accounts/claude/work/remove", tok, nil, nil); code != http.StatusOK {
		t.Fatalf("remove: %d %+v", code, e)
	}
	if code, e := f.api(t, "POST", "/runners/r1/logins", tok, hubapi.LoginRequest{Harness: "claude", Account: "work", Add: true}, nil); code != http.StatusCreated {
		t.Fatalf("add: %d %+v", code, e)
	}
	if cs := removals(f.mustSync(t, "r1", cred, later(on))); len(cs) != 0 {
		t.Fatalf("the removal went out after the account was added again: %+v", cs)
	}
}

// An account health still lists is not gone because the runner's document,
// not yet rebuilt since the account was added, leaves it out.
func TestARemovalWaitsForHealthAsWellAsTheDocument(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	on := managing("r1", "work")
	f.mustSync(t, "r1", cred, on)
	if code, e := f.api(t, "POST", "/runners/r1/accounts/claude/new/remove", tok, nil, nil); code != http.StatusOK {
		t.Fatalf("remove: %d %+v", code, e)
	}
	// Health already has the account; the document is from before it.
	if cs := removals(f.mustSync(t, "r1", cred, later(withAccounts(on, "work", "new")))); len(cs) != 1 || cs[0].Account != "new" {
		t.Fatalf("controls %+v, want the removal of new sent", cs)
	}
}
