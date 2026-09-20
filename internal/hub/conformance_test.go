package hub

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/conformance"
	"github.com/skkap/yad/internal/hub/store"
)

// `yad hub` is the reference implementation of the server half of v1, and the
// conformance suite is how any hub is checked against it. This is the one
// place the two meet: over HTTP, on a local listener, with the suite holding
// nothing but a URL and a registration token (ARCHITECTURE.md §7).
func TestYadHubPassesTheConformanceSuite(t *testing.T) {
	// The suite waits a lease out in real time, because a hub it does not own
	// shares no clock with it. Five seconds is the shortest lease this hub can
	// name and still pass the suite's own timings rule — a lease under the
	// interval is a finding — and it keeps the check at about six seconds
	// rather than the minute a shipped lease would cost every CI run.
	LeaseForTests = MinSyncInterval
	t.Cleanup(func() { LeaseForTests = 0 })

	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	srv := httptest.NewServer(New(Options{Store: s, SyncInterval: MinSyncInterval}))
	t.Cleanup(srv.Close)

	tok, _, err := IssueRegistrationToken(ctx, s, time.Hour, time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
	// Two runs for the suite's own harness: one carries the event and result
	// rules, the other is left unrenewed so its lease lapses. Without them the
	// suite skips half of §2 and says so, which would pass this test while
	// checking nothing — the loop at the end is what makes that impossible.
	for i := range 2 {
		run := v1.Run{
			RunID:   fmt.Sprintf("conformance-%d", i),
			Session: v1.SessionRef{ID: fmt.Sprintf("conformance-session-%d", i), New: true},
			Harness: conformance.DefaultHarness, Model: "none",
			Brief: v1.Brief{Instruction: "the conformance suite claims this run to check the protocol and refuses it"},
		}
		if err := s.EnqueueRun(ctx, run, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := conformance.Run(ctx, conformance.Options{
		BaseURL: srv.URL + BasePath, Token: tok, LeaseWait: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	rep.Print(&out)
	if rep.Failed() {
		t.Errorf("`yad hub` breaks a rule of the protocol it is the reference for:\n%s", out.String())
	}
	for _, o := range rep.Outcomes {
		if o.Status == conformance.Skipped {
			t.Errorf("%s was skipped, and this hub was given everything that check needs: %s", o.ID, o.Detail)
		}
	}
}
