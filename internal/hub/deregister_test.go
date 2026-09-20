package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

func (f *fixture) deregister(t *testing.T, runner, cred, reason string) (*http.Response, v1.ErrorEnvelope) {
	t.Helper()
	body := `{}`
	if reason != "" {
		body = `{"reason":` + quote(reason) + `}`
	}
	return post(t, f.hub, "/v1/runners/"+runner+"/deregister", body, headers(cred))
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// A runner that disconnects: what it held is lost, what it was offered and
// never claimed goes back in the queue, and its credential stops working.
func TestDeregisterLosesWhatTheRunnerHeld(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.enqueue(t, run("held", "s1"), run("offered", "s2"))

	// One run claimed, and the other offered in the sync that listed it: an
	// offer in flight when the runner disconnects.
	f.mustSync(t, "r1", cred, first("r1", 1))
	f.mustSync(t, "r1", cred, req("r1", 1, claimed("held")...))
	if got := f.state(t, "held"); got != "claimed" {
		t.Fatalf("held run is %s", got)
	}
	if got := f.state(t, "offered"); got != "offered" {
		t.Fatalf("offered run is %s", got)
	}

	res, env := f.deregister(t, "r1", cred, "the owner ran `yad disconnect`")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("deregister: %d %+v", res.StatusCode, env)
	}
	if got := f.state(t, "held"); got != "lost" {
		t.Errorf("the run it held is %s, want lost", got)
	}
	if got := f.state(t, "offered"); got != "queued" {
		t.Errorf("a run it never claimed is %s, want queued for another runner", got)
	}
	// The reason says who went and why, so a submitter reading the run knows.
	row, err := f.store.GetRun(context.Background(), "held")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(row.Reason.String, "r1 deregistered") || !strings.Contains(row.Reason.String, "yad disconnect") {
		t.Errorf("lost reason = %q", row.Reason.String)
	}
	// The credential is retired: nothing authenticates as that runner again.
	if res, _ := f.sync(t, "r1", cred, req("r1", 1)); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("sync after deregister: %d, want 401", res.StatusCode)
	}
}

// The owner's retry has to be able to finish. A second deregister with the
// retired credential is answered 401 — the credential really is gone — which
// is what `yad disconnect` reads as already deregistered, so the retry goes on
// to remove the credential and the connection.
func TestDeregisterTwiceIsAnsweredAsAlreadyGone(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	if res, env := f.deregister(t, "r1", cred, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("first: %d %+v", res.StatusCode, env)
	}
	res, env := f.deregister(t, "r1", cred, "")
	if res.StatusCode != http.StatusUnauthorized || env.Error.Code != v1.CodeUnauthorized {
		t.Errorf("second: %d %+v, want 401", res.StatusCode, env)
	}
	if env.Error.NextAction == "" {
		t.Error("the refusal carries no next action")
	}
}

// Registering again under a token for that runner brings it back: the row,
// its sessions and its history stayed.
func TestARunnerMayRegisterAgainAfterDeregistering(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.enqueue(t, run("a", "s1"))
	f.mustSync(t, "r1", cred, first("r1", 1))
	f.mustSync(t, "r1", cred, req("r1", 0, claimed("a")...))
	if res, env := f.deregister(t, "r1", cred, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("deregister: %d %+v", res.StatusCode, env)
	}

	again := f.register(t, "r1")
	if again == cred {
		t.Error("registering again returned the retired credential")
	}
	f.mustSync(t, "r1", again, first("r1", 1))
	// The lost run stays lost: a run is not run twice.
	if got := f.state(t, "a"); got != "lost" {
		t.Errorf("the run it held before is %s, want lost", got)
	}
}

// One runner's credential deregisters that runner alone.
func TestDeregisterIsNotAnotherRunnersToDo(t *testing.T) {
	f := newFixture(t)
	mine := f.register(t, "r1")
	theirs := f.register(t, "r2")
	f.enqueue(t, run("a", "s1"))
	f.mustSync(t, "r2", theirs, first("r2", 1))
	f.mustSync(t, "r2", theirs, req("r2", 0, claimed("a")...))

	res, env := f.deregister(t, "r2", mine, "")
	if res.StatusCode != http.StatusForbidden || env.Error.Code != v1.CodeUnauthorized {
		t.Errorf("deregistering another runner: %d %+v, want 403", res.StatusCode, env)
	}
	if got := f.state(t, "a"); got != "claimed" {
		t.Errorf("the other runner's run is %s — it was settled by a request that was refused", got)
	}
	if res, _ := f.sync(t, "r2", theirs, req("r2", 0, claimed("a")...)); res.StatusCode != http.StatusOK {
		t.Error("the other runner's credential stopped working")
	}
}

// A runner is untrusted input to a hub: its parting words end up in every run
// it held, where a person reads them.
func TestDeregisterReasonIsCleanedAndBounded(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.enqueue(t, run("a", "s1"))
	f.mustSync(t, "r1", cred, first("r1", 1))
	f.mustSync(t, "r1", cred, req("r1", 0, claimed("a")...))

	if res, env := f.deregister(t, "r1", cred, "line one\nline two\x00"+strings.Repeat("x", 500)); res.StatusCode != http.StatusOK {
		t.Fatalf("deregister: %d %+v", res.StatusCode, env)
	}
	row, err := f.store.GetRun(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	why := row.Reason.String
	if strings.ContainsAny(why, "\n\x00") {
		t.Errorf("reason carries a control character: %q", why)
	}
	if len(why) > maxDeregisterReason+len("runner r1 deregistered while it held this run: ")+len("…") {
		t.Errorf("reason is %d bytes: %q", len(why), why)
	}
}

func TestCleanReason(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"trimmed", "  stopping  ", "stopping"},
		{"newlines become spaces", "a\nb\tc", "a b c"},
		{"control characters go", "a\x01b", "ab"},
		{"multi-byte text survives", "やめます", "やめます"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanReason(tc.in); got != tc.want {
				t.Errorf("cleanReason(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// A cut never lands mid-rune: the reason is stored as JSON text.
	long := cleanReason(strings.Repeat("あ", 300))
	if len(long) > maxDeregisterReason+len("…") || !strings.HasSuffix(long, "…") {
		t.Errorf("a long reason came back %d bytes: %q", len(long), long)
	}
}

// Deregistering keeps the runner's row because its sessions and runs point at
// it — which is a reason only while SQLite is enforcing those references. It
// does not by default; the pragma that turns it on is in store.OpenSQLite, and
// the hub database inherits it. Watching the delete be refused is what keeps
// that reason true: an open that lost the pragma would fail here, rather than
// quietly turn deregistration into a way to orphan a session.
func TestTheRunnerRowCannotBeDeletedWhileItsWorkPointsAtIt(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.enqueue(t, run("a", "s1"))
	f.mustSync(t, "r1", cred, first("r1", 1))
	f.mustSync(t, "r1", cred, req("r1", 0, claimed("a")...))
	if res, env := f.deregister(t, "r1", cred, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("deregister: %d %+v", res.StatusCode, env)
	}

	_, err := f.store.DB.ExecContext(context.Background(), `DELETE FROM runners WHERE id = 'r1'`)
	if err == nil {
		t.Fatal("the runner row was deleted while a session and a run still reference it — deregistering by deleting it would orphan them")
	}
	// Named, so a delete refused for some other reason cannot pass for this.
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Errorf("the delete failed, but not on the reference: %v", err)
	}
}

// A session is resumable only on the runner holding it, so when that runner
// deregisters the session closes and the runs waiting in it are cancelled.
// Left open and bound, a queued run in one is offerable to nobody — the offer
// query takes only sessions unbound or bound to the asking runner, and a
// queued run has no lease to lapse — so it would wait for ever.
func TestDeregisterClosesTheRunnersSessionsAndCancelsWhatWaited(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cred := f.register(t, "r1")
	f.enqueue(t, run("first", "s1"))
	f.mustSync(t, "r1", cred, first("r1", 1))
	f.mustSync(t, "r1", cred, req("r1", 0, claimed("first")...))
	// A second run of the same session, queued behind the one being held.
	f.enqueue(t, continues("second", "s1"))
	if got := f.state(t, "second"); got != "queued" {
		t.Fatalf("the second run is %s", got)
	}

	if res, env := f.deregister(t, "r1", cred, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("deregister: %d %+v", res.StatusCode, env)
	}

	sess, err := f.store.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !sess.ClosedAt.Valid {
		t.Error("the session is still open, so nothing will ever take a run in it")
	}
	if got := sess.CloseReason.String; got != string(v1.SessionClosedByOwner) {
		t.Errorf("close reason = %q, want the same reason the runner records", got)
	}
	if got := f.state(t, "second"); got != "cancelled" {
		t.Errorf("the run waiting in it is %s, want cancelled — it is offerable to nobody", got)
	}
	row, err := f.store.GetRun(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(row.Reason.String, "new session") {
		t.Errorf("cancel reason = %q, want the next action", row.Reason.String)
	}
	// The run it held is still lost, not cancelled: it ran.
	if got := f.state(t, "first"); got != "lost" {
		t.Errorf("the run it held is %s, want lost", got)
	}
}

// Another runner's sessions are not this runner's to close.
func TestDeregisterClosesOnlyItsOwnSessions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mine := f.register(t, "r1")
	theirs := f.register(t, "r2")
	f.enqueue(t, run("mine", "s-mine"), run("theirs", "s-theirs"))
	f.mustSync(t, "r1", mine, first("r1", 1))
	f.mustSync(t, "r1", mine, req("r1", 0, claimed("mine")...))
	f.mustSync(t, "r2", theirs, first("r2", 1))
	f.mustSync(t, "r2", theirs, req("r2", 0, claimed("theirs")...))

	if res, env := f.deregister(t, "r1", mine, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("deregister: %d %+v", res.StatusCode, env)
	}
	theirSession, err := f.store.GetSession(ctx, "s-theirs")
	if err != nil {
		t.Fatal(err)
	}
	if theirSession.ClosedAt.Valid {
		t.Error("one runner deregistering closed another runner's session")
	}
	if got := f.state(t, "theirs"); got != "claimed" {
		t.Errorf("the other runner's run is %s", got)
	}
}
