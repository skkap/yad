package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/logfile"
)

// recordsAt makes log records the ring would have kept, newest last.
func recordsAt(base time.Time, recs ...logfile.Record) func() []logfile.Record {
	out := make([]logfile.Record, len(recs))
	for i, r := range recs {
		if r.Time.IsZero() {
			r.Time = base.Add(time.Duration(i) * time.Second)
		}
		if r.Level == 0 {
			r.Level = slog.LevelWarn
		}
		out[i] = r
	}
	return func() []logfile.Record { return out }
}

// The hazard this field is: recent_errors is error text sent to every hub, and
// HarnessReport.Error had to be cleaned of quoted child output after it
// shipped (DEV-60) while Warnings still carries it (DEV-67). A log record's
// attrs are where an exec error's wrapped text, the owner's home directory and
// a proxy URL with a password in it end up. None of it goes on the wire.
func TestHealthErrorsCarryTheMessageAndNeverTheAttrs(t *testing.T) {
	e := newEnv(t)
	l := healthLoop(t, e)
	const secret = "sk-ant-planted-0123456789"
	l.RecentErrors = recordsAt(e.clock.Now().Add(-time.Minute), logfile.Record{
		Level:   slog.LevelError,
		Message: "could not reach the hub; runs already held keep running and their results are kept until it answers",
		// Exactly the shape of a wrapped error from a child or an HTTP
		// client: the credential, a path on the machine, a proxied URL.
		Attrs: fmt.Sprintf("err=Post \"https://user:%s@proxy.internal/v1/runners/r1/sync\": dial tcp: no route to host home=%s", secret, e.paths.Data),
	})

	h := l.health(context.Background(), l.Pool.Reserve(l.Connection))
	if len(h.RecentErrors) != 1 {
		t.Fatalf("recent_errors = %q, want one entry", h.RecentErrors)
	}
	if !strings.Contains(h.RecentErrors[0], "could not reach the hub") {
		t.Errorf("recent_errors dropped the runner's own words: %q", h.RecentErrors[0])
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	for what, needle := range map[string]string{
		"a credential":                 secret,
		"a path on the machine":        e.paths.Data,
		"the proxy the runner dialled": "proxy.internal",
		"what the transport said":      "no route to host",
	} {
		if strings.Contains(string(b), needle) {
			t.Errorf("health carries %s: %s", what, b)
		}
	}
}

// The level and the time ride with the message: a hub reading "not ready" an
// hour after the fact must be able to tell that from one failing now, and a
// warning from an error.
func TestHealthErrorsCarryTheirTimeAndLevel(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	got := healthErrors([]logfile.Record{
		{Time: now.Add(-90 * time.Second), Level: slog.LevelError, Message: "the hub refused this runner's credential"},
	}, now)
	want := "2026-09-19T11:58:30Z ERROR the hub refused this runner's credential"
	if len(got) != 1 || got[0] != want {
		t.Errorf("recent_errors = %q, want [%q]", got, want)
	}
}

func TestHealthErrorsAreBounded(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	msg := func(i int) string { return fmt.Sprintf("problem number %d", i) }
	at := func(d time.Duration, m string) logfile.Record {
		return logfile.Record{Time: now.Add(d), Level: slog.LevelWarn, Message: m}
	}
	tests := []struct {
		name  string
		recs  []logfile.Record
		want  []string
		count int
	}{
		{
			name:  "the newest come first and the rest are dropped",
			recs:  []logfile.Record{at(-6*time.Minute, msg(1)), at(-5*time.Minute, msg(2)), at(-4*time.Minute, msg(3)), at(-3*time.Minute, msg(4)), at(-2*time.Minute, msg(5)), at(-time.Minute, msg(6))},
			count: maxHealthErrors,
			want:  []string{msg(6), msg(5), msg(4), msg(3), msg(2)},
		},
		{
			name:  "older than the window is not recent",
			recs:  []logfile.Record{at(-2*healthErrorWindow, msg(1)), at(-time.Minute, msg(2))},
			count: 1,
			want:  []string{msg(2)},
		},
		{
			name:  "a repeat says no more than its newest copy",
			recs:  []logfile.Record{at(-3*time.Minute, msg(1)), at(-2*time.Minute, msg(1)), at(-time.Minute, msg(2))},
			count: 2,
			want:  []string{msg(2), msg(1)},
		},
		{
			name:  "a record with no message is nothing to report",
			recs:  []logfile.Record{at(-time.Minute, "")},
			count: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := healthErrors(tc.recs, now)
			if len(got) != tc.count {
				t.Fatalf("recent_errors = %q, want %d entries", got, tc.count)
			}
			for i, w := range tc.want {
				if !strings.HasSuffix(got[i], w) {
					t.Errorf("recent_errors[%d] = %q, want it to end in %q", i, got[i], w)
				}
			}
		})
	}
}

// The ring holds records in the order they arrived, which is not the order
// they happened: slog stamps a record when the call is made and Recent.add
// takes its lock afterwards, so two goroutines logging at once land in either
// order. recent_errors promises newest first with no condition attached, so
// the order on the wire is the records' own times.
func TestHealthErrorsAreOrderedByWhenTheyHappened(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration, m string) logfile.Record {
		return logfile.Record{Time: now.Add(d), Level: slog.LevelWarn, Message: m}
	}
	// Arrival order puts the newest record in the middle and the oldest last:
	// what a delayed handler does to a ring.
	got := healthErrors([]logfile.Record{
		at(-3*time.Minute, "second"),
		at(-time.Minute, "newest"),
		at(-5*time.Minute, "oldest"),
	}, now)
	want := []string{"newest", "second", "oldest"}
	if len(got) != len(want) {
		t.Fatalf("recent_errors = %q", got)
	}
	for i, w := range want {
		if !strings.HasSuffix(got[i], w) {
			t.Errorf("recent_errors[%d] = %q, want it to end in %q", i, got[i], w)
		}
	}
}

// The seed of the sort, which the four lines of comment above it are the only
// other thing holding in place. Records sharing a Time are the case the stable
// sort cannot decide, and the copy starts newest-arrival-first so that arrival
// order — the only other evidence of which happened later — survives. Replace
// that copy with a plain one and nothing else in this file notices: every
// other record here carries a distinct Time.
//
// Two records at one moment, one message, different levels: whichever level
// comes back says which copy the dedup kept.
func TestRecordsSharingATimestampKeepArrivalOrder(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Minute)
	got := healthErrors([]logfile.Record{
		{Time: at, Level: slog.LevelWarn, Message: "the hub answered slowly"},
		{Time: at, Level: slog.LevelError, Message: "the hub answered slowly"},
	}, now)
	if len(got) != 1 {
		t.Fatalf("recent_errors = %q, want the repeat collapsed to one", got)
	}
	if !strings.Contains(got[0], "ERROR") {
		t.Errorf("recent_errors = %q; with the timestamps equal the later arrival is the one to keep", got)
	}
}

// The same hazard where it costs information rather than order: of two copies
// of one message, the one kept has to be the one that happened later, not the
// one that reached the ring later.
func TestARepeatKeepsTheCopyThatHappenedLast(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	got := healthErrors([]logfile.Record{
		{Time: now.Add(-time.Minute), Level: slog.LevelWarn, Message: "could not reach the hub"},
		{Time: now.Add(-9 * time.Minute), Level: slog.LevelWarn, Message: "could not reach the hub"},
	}, now)
	want := now.Add(-time.Minute).Format(time.RFC3339)
	if len(got) != 1 || !strings.HasPrefix(got[0], want) {
		t.Errorf("recent_errors = %q, want one entry stamped %s", got, want)
	}
}

// The ring bounds a record at 2 KiB for `yad status`, which is a local read.
// The wire gets less: five of these is the whole block's worst case.
func TestALongMessageIsCutForTheWire(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	got := healthErrors([]logfile.Record{{Time: now, Level: slog.LevelWarn, Message: strings.Repeat("é", 4<<10)}}, now)
	if len(got) != 1 {
		t.Fatalf("recent_errors = %d entries, want one", len(got))
	}
	// The prefix is the time and the level; the message itself is what is cut.
	prefix := now.Format(time.RFC3339) + " WARN "
	msg, found := strings.CutPrefix(got[0], prefix)
	if !found {
		t.Fatalf("entry %q does not start with %q", got[0], prefix)
	}
	if len(msg) > maxHealthErrorMessage+len("…") {
		t.Errorf("message is %d bytes, want at most %d", len(msg), maxHealthErrorMessage+len("…"))
	}
	if !strings.HasSuffix(msg, "…") {
		t.Errorf("a cut message does not say it was cut: %q", msg)
	}
	if !strings.HasPrefix(msg, "é") {
		t.Errorf("the cut split a rune: %q", msg[:8])
	}
}

// A runner with no ring configured — nothing has wired one in — sends the
// field absent rather than empty, which is what omitempty is for.
func TestNoRingMeansNoRecentErrors(t *testing.T) {
	e := newEnv(t)
	l := healthLoop(t, e)
	h := l.health(context.Background(), l.Pool.Reserve(l.Connection))
	if h.RecentErrors != nil {
		t.Errorf("recent_errors = %q, want absent", h.RecentErrors)
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "recent_errors") {
		t.Errorf("an empty recent_errors is on the wire: %s", b)
	}
}

// Health rides every sync, so its size is paid for on every one of them. The
// caps bound the block, and a hub is never told a harness is unusable because
// the accounts that could run were past the cap.
func TestAccountsAndWindowsAreCappedAndReadyIsNot(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var labels []string
	for i := range maxHealthAccounts + 4 {
		labels = append(labels, fmt.Sprintf("acct-%02d", i))
	}
	for _, label := range labels {
		if _, err := account.Ensure(e.paths.Data, "claude", label); err != nil {
			t.Fatal(err)
		}
	}
	// The first sixteen need login; the four past the cap have no store row
	// at all, and an account the store has never heard of reads free. So four
	// accounts can run and every one of them is past what a plain truncation
	// would keep.
	//
	// What this establishes is the end-to-end claim and not an ordering:
	// a harness whose free accounts all sit past the cap is still reported
	// ready, with the cap and the window cap both holding. It cannot prove
	// that Ready is computed before capAccounts rather than after, and since
	// the last-slot replacement neither can anything else — capAccounts puts
	// the account Soonest chose into the list whenever there is one, so
	// "a free account among all reports" and "a free account among those
	// reported" became the same condition and a Ready derived from either
	// reads the same. The ordering survives in the code as the clearer way to
	// write it, not as a thing a mutation could break.
	for _, label := range labels[:maxHealthAccounts] {
		if err := account.SetState(ctx, e.store.Queries, "claude", label, v1.AccountNeedsLogin, e.clock.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var windows []v1.AccountWindow
	for i := range maxHealthWindows + 3 {
		windows = append(windows, v1.AccountWindow{Name: fmt.Sprintf("window-%d", i), UsedPercent: float64(i)})
	}
	if err := account.SetWindows(ctx, e.store.Queries, "claude", labels[0], windows, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	l := healthLoop(t, e, labels...)

	h := l.health(ctx, l.Pool.Reserve(l.Connection))
	if len(h.Harnesses) != 1 {
		t.Fatalf("health harnesses = %+v", h.Harnesses)
	}
	hh := h.Harnesses[0]
	if len(hh.Accounts) != maxHealthAccounts {
		t.Errorf("accounts = %d, want the cap of %d", len(hh.Accounts), maxHealthAccounts)
	}
	if len(hh.Accounts[0].Windows) != maxHealthWindows {
		t.Errorf("windows = %d, want the cap of %d", len(hh.Accounts[0].Windows), maxHealthWindows)
	}
	if !hh.Ready {
		t.Error("a harness with a free account past the cap is reported not ready; the cap must cost detail, not correctness")
	}
	// The head of the owner's reporting order is what is kept — not a claim
	// about which account a run picks, which account.Soonest decides by the
	// soonest reset with the owner's order only breaking ties (decision 0039).
	// The last slot is the exception, and the test below is about it.
	if hh.Accounts[0].Label != labels[0] || hh.Accounts[maxHealthAccounts-2].Label != labels[maxHealthAccounts-2] {
		t.Errorf("accounts kept = %q…%q, want the head of the owner's order", hh.Accounts[0].Label, hh.Accounts[maxHealthAccounts-2].Label)
	}
	// The fixture claim above, asserted rather than described: with all four
	// free accounts undated, Soonest breaks the tie on the owner's order and
	// takes labels[16], which lands in the last slot.
	if got := hh.Accounts[maxHealthAccounts-1].Label; got != labels[maxHealthAccounts] {
		t.Errorf("last slot is %q, want the free account Soonest chose, %q", got, labels[maxHealthAccounts])
	}
}

// A hub must never read Ready true above a list in which every account named
// is limited or needs a login. That is not an incomplete report, it is a
// self-contradictory one, and a hub operator asking the question E8 exists to
// answer — why is this runner idle — would read it as a fault in yad.
//
// So the account a run would actually take is named whatever position the
// owner gave it: past the cap it takes the last kept slot.
func TestTheAccountCarryingTheWorkIsNamedEvenPastTheCap(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// A limit is judged against the wall clock, not the loop's: account.Load
	// takes no clock and Report calls time.Now(). A reset dated from the fake
	// clock is already in the past, so the account reads free and the test
	// proves nothing — which is what the first draft of this file did.
	realNow := time.Now()
	var labels []string
	for i := range maxHealthAccounts + 4 {
		labels = append(labels, fmt.Sprintf("acct-%02d", i))
	}
	for _, label := range labels {
		if _, err := account.Ensure(e.paths.Data, "claude", label); err != nil {
			t.Fatal(err)
		}
	}
	// Everything the cap would keep needs login, and everything past it bar
	// one is limited: the single free account sits at the cap's own boundary.
	for _, label := range labels[:maxHealthAccounts] {
		if err := account.SetState(ctx, e.store.Queries, "claude", label, v1.AccountNeedsLogin, e.clock.Now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, label := range labels[maxHealthAccounts+1:] {
		if err := account.SetLimit(ctx, e.store.Queries, "claude", label, realNow.Add(time.Hour), realNow); err != nil {
			t.Fatal(err)
		}
	}
	free := labels[maxHealthAccounts]
	l := healthLoop(t, e, labels...)

	hh := l.health(ctx, l.Pool.Reserve(l.Connection)).Harnesses[0]
	if !hh.Ready || len(hh.Accounts) != maxHealthAccounts {
		t.Fatalf("harness = ready %v with %d accounts", hh.Ready, len(hh.Accounts))
	}
	named := make(map[string]v1.AccountState, len(hh.Accounts))
	for _, a := range hh.Accounts {
		named[a.Label] = a.State
	}
	if named[free] != v1.AccountFree {
		t.Errorf("health says ready and names no account that can run: %+v", hh.Accounts)
	}
	// The last slot, not the first: the head of the report stays the head of
	// the owner's own order, and one slot means something specific.
	if got := hh.Accounts[maxHealthAccounts-1].Label; got != free {
		t.Errorf("the account carrying the work is at %q; want it in the last slot, %q", got, free)
	}
	if hh.Accounts[0].Label != labels[0] {
		t.Errorf("the report no longer starts at the head of the owner's order: %q", hh.Accounts[0].Label)
	}
}

// The overwhelmingly common case, and the one a change like this breaks: with
// the account a run would take already inside the cap, nothing is replaced,
// nothing is reordered, and the report is what it was before. A fix that
// fires when it should not is worse than the case it was written for.
func TestAnAccountInsideTheCapIsNotMoved(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// A limit is judged against the wall clock, not the loop's: account.Load
	// takes no clock and Report calls time.Now(). A reset dated from the fake
	// clock is already in the past, so the account reads free and the test
	// proves nothing — which is what the first draft of this file did.
	realNow := time.Now()
	var labels []string
	for i := range maxHealthAccounts + 4 {
		labels = append(labels, fmt.Sprintf("acct-%02d", i))
	}
	for _, label := range labels {
		if _, err := account.Ensure(e.paths.Data, "claude", label); err != nil {
			t.Fatal(err)
		}
	}
	// Free at the front, so Soonest picks inside the cap; everything else
	// limited, so there is something past the cap that could have been moved.
	for _, label := range labels[1:] {
		if err := account.SetLimit(ctx, e.store.Queries, "claude", label, realNow.Add(time.Hour), realNow); err != nil {
			t.Fatal(err)
		}
	}
	l := healthLoop(t, e, labels...)

	hh := l.health(ctx, l.Pool.Reserve(l.Connection)).Harnesses[0]
	if !hh.Ready || len(hh.Accounts) != maxHealthAccounts {
		t.Fatalf("harness = ready %v with %d accounts", hh.Ready, len(hh.Accounts))
	}
	for i, a := range hh.Accounts {
		if a.Label != labels[i] {
			t.Errorf("account %d is %q, want the owner's order unchanged at %q", i, a.Label, labels[i])
		}
	}
}

// And with nothing that can run, there is no contradiction to remove: sixteen
// limited rows under Ready false are the honest answer, and the report is the
// plain head of the owner's order. Soonest returns free accounts only, so this
// case never reaches the replacement at all.
func TestNoReplacementWhenNothingCanRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// A limit is judged against the wall clock, not the loop's: account.Load
	// takes no clock and Report calls time.Now(). A reset dated from the fake
	// clock is already in the past, so the account reads free and the test
	// proves nothing — which is what the first draft of this file did.
	realNow := time.Now()
	var labels []string
	for i := range maxHealthAccounts + 4 {
		labels = append(labels, fmt.Sprintf("acct-%02d", i))
	}
	for _, label := range labels {
		if _, err := account.Ensure(e.paths.Data, "claude", label); err != nil {
			t.Fatal(err)
		}
		if err := account.SetLimit(ctx, e.store.Queries, "claude", label, realNow.Add(time.Hour), realNow); err != nil {
			t.Fatal(err)
		}
	}
	l := healthLoop(t, e, labels...)

	hh := l.health(ctx, l.Pool.Reserve(l.Connection)).Harnesses[0]
	// The length first: the loop below is a no-op over a short slice, so
	// without this a capAccounts that returned nothing on the empty-label
	// path would leave this test green while the block it is named for is
	// gone. Its two siblings assert it; this one did not.
	if hh.Ready || len(hh.Accounts) != maxHealthAccounts {
		t.Fatalf("harness = ready %v with %d accounts, want not ready with %d", hh.Ready, len(hh.Accounts), maxHealthAccounts)
	}
	for i, a := range hh.Accounts {
		if a.Label != labels[i] {
			t.Errorf("account %d is %q, want the owner's order unchanged at %q", i, a.Label, labels[i])
		}
	}
}

// The whole of §2's health block, in one document: every field either carries
// something or is deliberately absent. A field added to v1.Health and left
// unset on the runner fails here rather than reaching a hub as a zero.
func TestEverySyncCarriesEveryHealthField(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := account.Ensure(e.paths.Data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if err := account.SetWindows(ctx, e.store.Queries, "claude", "work",
		[]v1.AccountWindow{{Name: "five_hour", UsedPercent: 96}}, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	l := healthLoop(t, e, "work")
	e.collector(l)
	l.RecentErrors = recordsAt(e.clock.Now().Add(-time.Minute), logfile.Record{Message: "a workdir could not be reclaimed"})

	h := l.health(ctx, l.Pool.Reserve(l.Connection))
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	// Why a field may be missing from the document, when it may.
	absent := map[string]string{
		"draining": "this runner is not draining, and omitempty keeps a false off the wire",
	}
	for i := range reflect.TypeFor[v1.Health]().NumField() {
		name, _, _ := strings.Cut(reflect.TypeFor[v1.Health]().Field(i).Tag.Get("json"), ",")
		why, deliberate := absent[name]
		delete(absent, name)
		_, present := doc[name]
		switch {
		case present && deliberate:
			t.Errorf("health carries %s, which this runner was expected to leave out: %s", name, why)
		case !present && !deliberate:
			t.Errorf("health has no %s, and no reason is recorded for it being absent", name)
		case !present:
			t.Logf("%s is absent: %s", name, why)
		}
	}
	for name := range absent {
		t.Errorf("a reason is recorded for %s being absent, and v1.Health has no such field", name)
	}

	// Populated, not merely present.
	if h.FreeCapacity.Total != 1 || h.DiskFreeBytes <= 0 {
		t.Errorf("free capacity %+v, disk free %d", h.FreeCapacity, h.DiskFreeBytes)
	}
	if math.IsNaN(h.Load) || math.IsInf(h.Load, 0) || h.Load < 0 {
		t.Errorf("load = %v", h.Load)
	}
	if len(h.RecentErrors) != 1 {
		t.Errorf("recent_errors = %q", h.RecentErrors)
	}
	if len(h.Harnesses) != 1 || !h.Harnesses[0].Ready || len(h.Harnesses[0].Accounts) != 1 {
		t.Fatalf("harnesses = %+v", h.Harnesses)
	}
	a := h.Harnesses[0].Accounts[0]
	if a.Label != "work" || a.State != v1.AccountFree || len(a.Windows) != 1 || a.Windows[0].UsedPercent != 96 {
		t.Errorf("account = %+v", a)
	}
	// A free account has no reset, and that absence is the news.
	if a.LimitedUntil != nil {
		t.Errorf("a free account carries limited_until = %v", a.LimitedUntil)
	}
}
