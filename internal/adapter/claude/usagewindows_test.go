package claude

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
)

// recorded is a fixture taken from a claude the version directory names,
// rather than derived from another one.
func recorded(t *testing.T, version, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "claude-"+version, name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func windowNamed(ws []adapter.Window, name string) (adapter.Window, bool) {
	for _, w := range ws {
		if w.Name == name {
			return w, true
		}
	}
	return adapter.Window{}, false
}

// The acceptance criterion, against a stream claude really wrote (DEV-27, the
// DEV-24 instrument): the API answered 429 once, claude retried it by itself
// and the turn succeeded.
//
// A rate limit is not a usage limit (DOMAIN.md): the retry is a metric and
// nothing more. Reading it as a limit would park a working account for five
// hours because the API hiccuped once.
//
// Recorded rather than written by hand because a stream written by hand is a
// stream that agrees with whatever the code already does. This one carries
// claude's own api_retry frame, its own delays and its own result.
func TestATransientRetryIsNotAUsageLimit(t *testing.T) {
	h := &harness{fixture: recorded(t, "2.1.278", "api-retry-then-success")}
	_, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunSucceeded {
		t.Fatalf("outcome %s (error %+v)", out.State, out.Error)
	}
	if out.Limit != nil {
		t.Errorf("a retried 429 produced a usage limit: %+v", out.Limit)
	}
	if out.APIRetries != 1 {
		t.Errorf("api retries = %d, want 1 — the stream carries one api_retry frame", out.APIRetries)
	}
}

// The other side of the same recording: a 429 that survived all ten of
// claude's own retries and ended the turn. Claude gave up rather than carried
// on, so this one is an exhausted account and not throttling, and it is a
// usage limit with no reset because a bare 429 carries none.
func TestA429ThatSurvivedEveryRetryIsAUsageLimit(t *testing.T) {
	h := &harness{fixture: recorded(t, "2.1.278", "usage-limit-429")}
	_, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunFailed || out.Error == nil || out.Error.Class != adapter.ClassUsageLimit {
		t.Fatalf("outcome %s (error %+v)", out.State, out.Error)
	}
	if out.Limit == nil {
		t.Fatal("no limit on a turn that failed at a usage limit")
	}
	if !out.Limit.ResetAt.IsZero() {
		t.Errorf("reset %s, want none — a bare 429 carries no reset time", out.Limit.ResetAt)
	}
	if out.APIRetries != 10 {
		t.Errorf("api retries = %d, want 10 — claude retried ten times before giving up", out.APIRetries)
	}
}

// A five-hour and a weekly limit, per the acceptance criteria. Both windows
// are reported either way: which one is exhausted decides the limit, and the
// other's headroom is what tells a hub when the account comes back for good.
func TestClaudeUsageLimitFixtures(t *testing.T) {
	fiveHour := time.Unix(1789764600, 0).UTC()
	weekly := time.Unix(1790186400, 0).UTC()
	for _, c := range []struct {
		name         string
		fixture      string
		window       string
		reset        time.Time
		fiveHourUsed float64
		weeklyUsed   float64
	}{
		{"five hour", "usage-limit-five-hour", "five_hour", fiveHour, 100, 47},
		{"weekly", "usage-limit-weekly", "seven_day", weekly, 12, 100},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := &harness{fixture: fixture(c.fixture)}
			_, out, _ := drive(t, context.Background(), h.spec(t), nil)
			if out.State != v1.RunFailed || out.Error == nil || out.Error.Class != adapter.ClassUsageLimit {
				t.Fatalf("outcome %s (error %+v)", out.State, out.Error)
			}
			if out.Limit == nil || out.Limit.Window != c.window || !out.Limit.ResetAt.Equal(c.reset) {
				t.Fatalf("limit %+v, want %s until %s", out.Limit, c.window, c.reset)
			}
			if len(out.Windows) != 2 {
				t.Fatalf("windows %+v, want both of claude's", out.Windows)
			}
			for _, w := range []struct {
				name  string
				used  float64
				reset time.Time
			}{{"five_hour", c.fiveHourUsed, fiveHour}, {"seven_day", c.weeklyUsed, weekly}} {
				got, ok := windowNamed(out.Windows, w.name)
				if !ok {
					t.Errorf("no %s window in %+v", w.name, out.Windows)
					continue
				}
				if got.UsedPercent != w.used {
					t.Errorf("%s used %v%%, want %v%%", w.name, got.UsedPercent, w.used)
				}
				if !got.ResetAt.Equal(w.reset) {
					t.Errorf("%s resets %s, want %s", w.name, got.ResetAt, w.reset)
				}
			}
		})
	}
}

// Claude reports a 0-1 fraction and the protocol a percentage. The conversion
// is pinned against a recording rather than against the constant that performs
// it: a factor of a hundred is invisible in a number on its own, and this is
// the number a hub would route on.
func TestClaudeUtilizationIsConvertedToPercent(t *testing.T) {
	// plain.jsonl carries claude's own rate_limit_event for an ordinary turn:
	// five_hour 0.33, seven_day 0.47, both "allowed".
	h := &harness{fixture: fixture("plain")}
	_, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunSucceeded {
		t.Fatalf("outcome %s (error %+v)", out.State, out.Error)
	}
	if out.Limit != nil {
		t.Errorf("an allowed rate_limit_event produced a limit: %+v", out.Limit)
	}
	for _, w := range []struct {
		name string
		want float64
	}{{"five_hour", 33}, {"seven_day", 47}} {
		got, ok := windowNamed(out.Windows, w.name)
		if !ok {
			t.Fatalf("no %s window in %+v", w.name, out.Windows)
		}
		if got.UsedPercent != w.want {
			t.Errorf("%s used %v, want %v — claude's 0-1 fraction times 100", w.name, got.UsedPercent, w.want)
		}
	}
}

// A window claude never mentioned is absent, not zero: zero use is what a
// fresh window reads, and a hub must not be told an account is untouched
// because nothing said otherwise.
func TestNoRateLimitEventMeansNoWindows(t *testing.T) {
	path := derive(t, "plain", func(lines []string) []string {
		out := lines[:0]
		for _, l := range lines {
			if !contains(l, `"type":"rate_limit_event"`) && !contains(l, `"type": "rate_limit_event"`) {
				out = append(out, l)
			}
		}
		return out
	})
	h := &harness{fixture: path}
	_, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunSucceeded {
		t.Fatalf("outcome %s (error %+v)", out.State, out.Error)
	}
	if out.Windows != nil {
		t.Errorf("windows %+v, want none", out.Windows)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The conversion is rounded because the multiplication is not exact in binary.
// Without it a 29% window reports 28.999999999999996, which a hub carries in
// its JSON and shows to somebody. Claude has been seen to report the fraction
// to four places, so two places of percent lose nothing.
func TestAwkwardUtilizationsRoundCleanly(t *testing.T) {
	for _, c := range []struct {
		utilization float64
		want        float64
	}{
		{0.29, 29}, // 0.29*100 is 28.999999999999996 in float64
		{0.33, 33},
		{0.7825, 78.25},
		{1, 100},
		{0, 0},
	} {
		tr := newTranslator("s", func(v1.Event) {})
		tr.recordWindows(&rateLimitInfo{
			Status:         "allowed",
			UnifiedWindows: map[string]unifiedWindow{"five_hour": {ResetsAt: 1789764600, Utilization: c.utilization}},
		})
		got := tr.windowList()
		if len(got) != 1 || got[0].UsedPercent != c.want {
			t.Errorf("utilization %v -> %+v, want %v%%", c.utilization, got, c.want)
		}
	}
}
