package codex

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
)

func windowNamed(ws []adapter.Window, name string) (adapter.Window, bool) {
	for _, w := range ws {
		if w.Name == name {
			return w, true
		}
	}
	return adapter.Window{}, false
}

// A five-hour and a weekly limit, per the acceptance criteria. Codex names its
// windows primary and secondary rather than by duration — their lengths vary
// by plan — and the fixtures carry the durations Codex reports alongside:
// 300 minutes and 10080.
//
// Both windows are reported whichever one ran out. Which is exhausted decides
// the limit; the other's headroom is what says whether the account comes back
// for good at that reset.
func TestCodexUsageLimitFixtures(t *testing.T) {
	primaryReset := time.Unix(1789803060, 0).UTC()
	secondaryReset := time.Unix(1790300000, 0).UTC()
	for _, c := range []struct {
		name                       string
		fixture                    string
		window                     string
		reset                      time.Time
		primaryUsed, secondaryUsed float64
	}{
		{"the five-hour window", "usage-limit", "primary", primaryReset, 100, 41},
		{"the weekly window", "usage-limit-weekly", "secondary", secondaryReset, 58, 100},
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
			for _, w := range []struct {
				name  string
				used  float64
				reset time.Time
			}{{"primary", c.primaryUsed, primaryReset}, {"secondary", c.secondaryUsed, secondaryReset}} {
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

// Codex already reports a percentage, so nothing is scaled: what the snapshot
// says is what a hub sees. Pinned against a fixture rather than against the
// line that copies it, because the harnesses disagree on the scale and a
// factor of a hundred is invisible in a number on its own.
func TestCodexUsedPercentIsNotRescaled(t *testing.T) {
	h := &harness{fixture: fixture("usage-limit")}
	_, out, _ := drive(t, context.Background(), h.spec(t), nil)
	got, ok := windowNamed(out.Windows, "secondary")
	if !ok {
		t.Fatalf("no secondary window in %+v", out.Windows)
	}
	if got.UsedPercent != 41 {
		t.Errorf("secondary used %v, want 41 — the snapshot's own usedPercent", got.UsedPercent)
	}
}

// The point of reporting windows from every run rather than only from a
// failed one: an ordinary turn that succeeded already knows how much of the
// account is spent. This is codex's own recorded plain turn, whose snapshot
// says the primary window is at 93% and names no secondary at all.
//
// A window the snapshot left null is absent from the report rather than zero:
// zero use is what a fresh window reads, and a hub must not be told an
// account is untouched because nothing said otherwise.
func TestASucceededTurnStillReportsItsWindows(t *testing.T) {
	h := &harness{fixture: fixture("plain")}
	_, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunSucceeded {
		t.Fatalf("outcome %s (error %+v)", out.State, out.Error)
	}
	if out.Limit != nil {
		t.Errorf("a turn that succeeded reported a usage limit: %+v", out.Limit)
	}
	if len(out.Windows) != 1 {
		t.Fatalf("windows %+v, want only the primary the snapshot names", out.Windows)
	}
	got := out.Windows[0]
	want := adapter.Window{Name: "primary", UsedPercent: 93, ResetAt: time.Unix(1789840314, 0).UTC()}
	if got.Name != want.Name || got.UsedPercent != want.UsedPercent || !got.ResetAt.Equal(want.ResetAt) {
		t.Errorf("window %+v, want %+v", got, want)
	}
}

// A turn Codex never sent a snapshot for reports no windows at all, rather
// than a set of zeroes nothing said.
func TestNoSnapshotMeansNoWindows(t *testing.T) {
	h := &harness{fixture: withoutLines(t, "plain", `"method":"account/rateLimits/updated"`)}
	_, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunSucceeded {
		t.Fatalf("outcome %s (error %+v)", out.State, out.Error)
	}
	if out.Windows != nil {
		t.Errorf("windows %+v, want none", out.Windows)
	}
}

// withoutLines drops every line of a fixture containing mark.
func withoutLines(t *testing.T, name, mark string) string {
	t.Helper()
	b, err := os.ReadFile(fixture(name))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if !strings.Contains(l, mark) {
			kept = append(kept, l)
		}
	}
	return writeFixture(t, strings.Join(kept, "\n")+"\n")
}

// Codex retries a transient failure by itself and says so with willRetry. A
// turn that then succeeds costs the account nothing: a rate limit is not a
// usage limit (DOMAIN.md), and nothing in the outcome may suggest it is.
func TestCodexTransientRetryIsNotAUsageLimit(t *testing.T) {
	path := insertBefore(t, "plain", `{"method":"turn/completed"`,
		`{"method":"error","params":{"error":{"message":"stream disconnected before completion","codexErrorInfo":null,"additionalDetails":null},"willRetry":true,"threadId":"01a0b878-c0c0-7783-b1a6-65eeb944100e","turnId":"01a0b878-c0ca-7562-880a-548f542e449d"},"emittedAtMs":1789801320000}`)
	h := &harness{fixture: path}
	_, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunSucceeded {
		t.Fatalf("outcome %s (error %+v)", out.State, out.Error)
	}
	if out.Limit != nil {
		t.Errorf("a retried transient failure produced a usage limit: %+v", out.Limit)
	}
	if out.APIRetries != 1 {
		t.Errorf("api retries = %d, want 1", out.APIRetries)
	}
}

// insertBefore puts a line into a fixture ahead of the first line starting
// with prefix, so a recorded conversation can carry one extra notification
// without being rewritten by hand.
func insertBefore(t *testing.T, name, prefix, line string) string {
	t.Helper()
	b, err := os.ReadFile(fixture(name))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, prefix)
	if i < 0 {
		t.Fatalf("%s has no %s", name, prefix)
	}
	return writeFixture(t, s[:i]+line+"\n"+s[i:])
}
