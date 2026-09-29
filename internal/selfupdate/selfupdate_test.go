package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/upgrade"
)

// fakeSource is a release in memory: nothing here reaches GitHub
// (ARCHITECTURE.md §7). It counts what it is asked, because one request per
// check, and no download of a release already refused, are part of the
// contract.
type fakeSource struct {
	mu        sync.Mutex
	tag       string
	binary    []byte
	latestErr error
	latests   int
	downloads int
}

func (f *fakeSource) Latest(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latests++
	return f.tag, f.latestErr
}

func (f *fakeSource) Download(_ context.Context, tag string, assets []string, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads++
	if tag != f.tag {
		return fmt.Errorf("no release %s", tag)
	}
	h := sha256.Sum256(f.binary)
	files := map[string][]byte{
		asset:                 f.binary,
		upgrade.ChecksumsName: fmt.Appendf(nil, "%s  %s\n", hex.EncodeToString(h[:]), asset),
	}
	for _, a := range assets {
		if err := os.WriteFile(filepath.Join(dir, a), files[a], 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeSource) counts() (latests, downloads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latests, f.downloads
}

const asset = "yad-linux-amd64"

// fakeClock moves only when the test says. After registers a waiter and
// announces it on asked, so a test can tell the updater is blocked on it.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []waiter
	asked chan time.Duration
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC), asked: make(chan time.Duration, 100)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	ch := make(chan time.Time, 1)
	c.waits = append(c.waits, waiter{c.now.Add(d), ch})
	c.mu.Unlock()
	c.asked <- d
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waits[:0]
	for _, w := range c.waits {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- c.now
	}
	c.waits = kept
}

// next is the wait the updater asks for next.
func (c *fakeClock) next(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-c.asked:
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("the updater never waited on the clock")
		return 0
	}
}

type harness struct {
	src    *fakeSource
	clock  *fakeClock
	target string
	about  buildinfo.About
	askErr error
	asked  int
	log    *bytes.Buffer
	mu     sync.Mutex
	idle   bool
	swaps  []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	target := filepath.Join(t.TempDir(), "yad")
	if err := os.WriteFile(target, []byte("v0.8.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &harness{
		src:    &fakeSource{tag: "v0.9.0", binary: []byte("v0.9.0")},
		clock:  newClock(),
		target: target,
		about:  buildinfo.About{Version: "v0.9.0", Protocols: []string{"1"}},
		log:    &bytes.Buffer{},
	}
}

func (h *harness) options() Options {
	return Options{
		Source: h.src, Target: h.target, GOOS: "linux", GOARCH: "amd64",
		Installed: "v0.8.0", Needs: map[string][]string{"1": {"home", "zumino"}},
		Ask: func(context.Context, string) (buildinfo.About, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.asked++
			return h.about, h.askErr
		},
		Idle: func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.idle
		},
		Swap: func(reason string) bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.swaps = append(h.swaps, reason)
			return true
		},
		Clock: h.clock,
		Rand:  func() float64 { return 0.5 },
		Log:   slog.New(slog.NewTextHandler(h.log, nil)),
	}
}

func (h *harness) installed(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(h.target)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (h *harness) setIdle(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.idle = v
}

func (h *harness) swapped() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.swaps...)
}

// A newer release that speaks what every connection needs is installed in
// place — through upgrade.Apply, so its checksum was checked — and waits to
// take over.
func TestCheckInstallsANewerRelease(t *testing.T) {
	h := newHarness(t)
	u := New(h.options())
	if !u.Check(context.Background()) {
		t.Fatalf("Check took nothing; log:\n%s", h.log)
	}
	if got := h.installed(t); got != "v0.9.0" {
		t.Errorf("installed binary is %q, want the release", got)
	}
	st := u.Status()
	if st.Pending == nil || st.Pending.Tag != "v0.9.0" || st.Pending.Swapping {
		t.Fatalf("pending = %+v, want v0.9.0 waiting", st.Pending)
	}
	if want := h.clock.Now().Add(IdleWait); !st.Pending.By.Equal(want) {
		t.Errorf("drains by %s, want 24 h after the install (%s)", st.Pending.By, want)
	}
	if st.Installed != "v0.8.0" || st.Latest != "v0.9.0" || st.LastError != "" || st.LastCheck.IsZero() {
		t.Errorf("status %+v", st)
	}
	if len(h.swapped()) != 0 {
		t.Error("Check began the takeover itself; only the wait for an idle moment does")
	}
}

// The owner's third decision: a release that no longer speaks a protocol
// major a connection syncs over is refused, the binary is kept, and the log
// and status say why. It is not downloaded again for the life of the process.
func TestCheckRefusesAReleaseThatDropsAConnectionsProtocol(t *testing.T) {
	h := newHarness(t)
	h.about.Protocols = []string{"2"}
	u := New(h.options())
	if u.Check(context.Background()) {
		t.Fatal("Check took a release that drops protocol v1")
	}
	if got := h.installed(t); got != "v0.8.0" {
		t.Errorf("installed binary is %q, want the one running", got)
	}
	st := u.Status()
	if st.Pending != nil || st.Refused == nil || st.Refused.Tag != "v0.9.0" {
		t.Fatalf("status %+v, want v0.9.0 refused", st)
	}
	for _, want := range []string{"no longer speaks protocol v1", "home, zumino", "it speaks v2", "keeps v0.8.0", "yad upgrade --tag v0.9.0"} {
		if !strings.Contains(st.Refused.Reason, want) {
			t.Errorf("reason %q does not say %q", st.Refused.Reason, want)
		}
	}
	if !strings.Contains(h.log.String(), "level=WARN") || !strings.Contains(h.log.String(), "refused") {
		t.Errorf("the log does not warn of the refusal:\n%s", h.log)
	}

	if u.Check(context.Background()) {
		t.Fatal("the second check took it")
	}
	latests, downloads := h.src.counts()
	if latests != 2 || downloads != 1 {
		t.Errorf("%d requests and %d downloads over two checks; want one request each and the refused release downloaded once", latests, downloads)
	}
	if u.Status().Refused == nil {
		t.Error("the refusal was forgotten at the next check")
	}
}

// A runner with no connection needs no protocol, and a release that speaks
// another is as good as any.
func TestAHublessRunnerNeedsNoProtocol(t *testing.T) {
	h := newHarness(t)
	h.about.Protocols = []string{"2"}
	o := h.options()
	o.Needs = nil
	if !New(o).Check(context.Background()) {
		t.Fatalf("Check refused a release a runner with no hub can use; log:\n%s", h.log)
	}
}

// A binary that does not call itself newer is refused: taken, it would be
// found behind again at the next check and downloaded for ever.
func TestCheckRefusesAMislabelledRelease(t *testing.T) {
	h := newHarness(t)
	h.about.Version = "v0.8.0"
	u := New(h.options())
	if u.Check(context.Background()) {
		t.Fatal("Check took a release whose binary is this build")
	}
	if r := u.Status().Refused; r == nil || !strings.Contains(r.Reason, `calls itself "v0.8.0"`) {
		t.Errorf("refused %+v", r)
	}
	if got := h.installed(t); got != "v0.8.0" {
		t.Errorf("installed binary is %q", got)
	}
}

// Absence is data: a check that gets no answer, or a binary that will not say
// what it is, is a warning in the log and in status, never a stop — and,
// unlike a refusal, is tried again at the next check.
func TestAFailedCheckIsAWarningAndIsTriedAgain(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*harness)
		want string
	}{
		{"no answer", func(h *harness) { h.src.latestErr = errors.New("could not reach github.com") }, "could not reach"},
		{"the binary does not answer", func(h *harness) { h.askErr = errors.New("`version --json` did not answer in 30s") }, "would not say what it is"},
		{"an unreadable tag", func(h *harness) { h.src.tag = "nightly" }, "not a version number"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			c.set(h)
			u := New(h.options())
			if u.Check(context.Background()) {
				t.Fatal("Check took a release")
			}
			st := u.Status()
			if !strings.Contains(st.LastError, c.want) || st.LastCheck.IsZero() || st.Refused != nil || st.Pending != nil {
				t.Errorf("status %+v, want the failure %q and nothing refused", st, c.want)
			}
			if got := h.installed(t); got != "v0.8.0" {
				t.Errorf("installed binary is %q", got)
			}
			if !strings.Contains(h.log.String(), "level=WARN") {
				t.Errorf("no warning logged:\n%s", h.log)
			}
			// Cleared, the next check succeeds.
			h.src.latestErr, h.askErr, h.src.tag = nil, nil, "v0.9.0"
			if !u.Check(context.Background()) || u.Status().LastError != "" {
				t.Errorf("the retry did not take the release: %+v", u.Status())
			}
		})
	}
}

// A release that is not newer is left alone, and nothing is downloaded.
func TestCheckLeavesAnUpToDateBuild(t *testing.T) {
	for _, installed := range []string{"v0.9.0", "v0.9.0-3-gabc1234", "v1.0.0"} {
		h := newHarness(t)
		o := h.options()
		o.Installed = installed
		u := New(o)
		if u.Check(context.Background()) {
			t.Errorf("%s: Check took v0.9.0", installed)
		}
		if _, downloads := h.src.counts(); downloads != 0 {
			t.Errorf("%s: downloaded %d times", installed, downloads)
		}
		if st := u.Status(); st.Latest != "v0.9.0" || st.LastError != "" {
			t.Errorf("%s: status %+v", installed, st)
		}
	}
}

// An unstamped build has nothing to compare, so it asks nothing and says so.
func TestAnUnstampedBuildChecksNothing(t *testing.T) {
	h := newHarness(t)
	o := h.options()
	o.Installed = "dev"
	u := New(o)
	if off := u.Status().Off; !strings.Contains(off, `"dev"`) || !strings.Contains(off, "yad upgrade --force") {
		t.Errorf("off = %q", off)
	}
	done := make(chan struct{})
	go func() { u.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run kept a schedule for a build it can never update")
	}
	if latests, _ := h.src.counts(); latests != 0 {
		t.Errorf("asked for the newest release %d times", latests)
	}
}

// The schedule: the first check soon after the start, then every six hours,
// each jittered by a tenth, one request per check.
func TestRunChecksEverySixHoursJittered(t *testing.T) {
	h := newHarness(t)
	h.src.tag = "v0.8.0" // nothing newer, so the schedule goes on
	o := h.options()
	r := []float64{0, 0.99, 0.5}
	i := 0
	var rmu sync.Mutex
	o.Rand = func() float64 {
		rmu.Lock()
		defer rmu.Unlock()
		v := r[i%len(r)]
		i++
		return v
	}
	u := New(o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	first := h.clock.next(t)
	if first != First-First/10 {
		t.Errorf("first check after %s, want %s: First less a tenth", first, First-First/10)
	}
	if next := u.Status().NextCheck; !next.Equal(h.clock.Now().Add(first)) {
		t.Errorf("status says the next check is at %s", next)
	}
	h.clock.Advance(first)
	for n, rand := range []float64{0.99, 0.5} {
		d := h.clock.next(t)
		want := Every + time.Duration((rand*2-1)*jitterFraction*float64(Every))
		if d != want {
			t.Errorf("check %d waited %s, want %s", n+2, d, want)
		}
		if d < Every-Every/10 || d > Every+Every/10 {
			t.Errorf("check %d waited %s, outside six hours and a tenth", n+2, d)
		}
		if latests, _ := h.src.counts(); latests != n+1 {
			t.Errorf("after %d checks, %d requests", n+1, latests)
		}
		h.clock.Advance(d)
	}
}

// The owner's fourth decision: work goes on while a release waits, and it
// takes over at the first moment nothing is in progress.
func TestARunnerTakesOverAtItsFirstIdleMoment(t *testing.T) {
	h := newHarness(t)
	u := New(h.options())
	done := make(chan struct{})
	go func() { u.Run(context.Background()); close(done) }()
	h.clock.Advance(h.clock.next(t)) // the first check installs the release
	for range 3 {
		h.clock.next(t) // a poll, busy
		if len(h.swapped()) != 0 {
			t.Fatal("took over while busy")
		}
		h.clock.Advance(Poll)
	}
	h.clock.next(t) // blocked on a poll, having found the runner busy
	h.setIdle(true)
	h.clock.Advance(Poll)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not end once the takeover began")
	}
	swaps := h.swapped()
	if len(swaps) != 1 || !strings.Contains(swaps[0], "v0.9.0") || !strings.Contains(swaps[0], "idle") {
		t.Errorf("swaps %q, want one for v0.9.0 at an idle moment", swaps)
	}
	if p := u.Status().Pending; p == nil || !p.Swapping || !strings.Contains(p.Reason, "idle") {
		t.Errorf("pending %+v, want it swapping", p)
	}
}

// With no idle moment in 24 hours the runner drains for the release: Swap is
// the same drain, and it is the drain that never interrupts a run.
func TestARunnerDrainsWhenNoIdleMomentComesInADay(t *testing.T) {
	h := newHarness(t)
	o := h.options()
	o.Poll = time.Hour // fewer turns of the loop; the deadline is what matters
	u := New(o)
	done := make(chan struct{})
	go func() { u.Run(context.Background()); close(done) }()
	h.clock.Advance(h.clock.next(t))
	for waited := time.Duration(0); waited < IdleWait; waited += time.Hour {
		h.clock.next(t)
		if len(h.swapped()) != 0 {
			t.Fatalf("drained %s after the install, before the day was up", waited)
		}
		h.clock.Advance(time.Hour)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not end at the deadline")
	}
	swaps := h.swapped()
	if len(swaps) != 1 || !strings.Contains(swaps[0], "no idle moment came in 24h0m0s") {
		t.Errorf("swaps %q, want one drain at the deadline", swaps)
	}
}

func TestParseAbout(t *testing.T) {
	for _, c := range []struct {
		name, out string
		want      buildinfo.About
		err       string
	}{
		{"this release", `{"version":"v0.9.0","commit":"abc1234","protocols":["1","2"]}`,
			buildinfo.About{Version: "v0.9.0", Commit: "abc1234", Protocols: []string{"1", "2"}}, ""},
		// A release older than --json ignores the flag and prints its line:
		// every one of them speaks v1 alone.
		{"a release before --json", "yad v0.4.0 (abc1234)\n",
			buildinfo.About{Version: "v0.4.0", Commit: "abc1234", Protocols: []string{"1"}}, ""},
		{"an unstamped old one", "yad dev\n", buildinfo.About{Version: "dev", Protocols: []string{"1"}}, ""},
		{"JSON without protocols", `{"version":"v0.9.0"}`, buildinfo.About{Version: "v0.9.0", Protocols: []string{"1"}}, ""},
		{"no version", `{"protocols":["1"]}`, buildinfo.About{}, "named no version"},
		{"not yad", "Usage: something else\n", buildinfo.About{}, "not what any yad version prints"},
		{"broken JSON", `{"version":`, buildinfo.About{}, "not yad's JSON"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseAbout([]byte(c.out))
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("ParseAbout = %+v, %v; want an error saying %q", got, err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Version != c.want.Version || got.Commit != c.want.Commit || strings.Join(got.Protocols, ",") != strings.Join(c.want.Protocols, ",") {
				t.Errorf("ParseAbout = %+v, want %+v", got, c.want)
			}
		})
	}
}

// Ask runs the binary with HOME alone: nothing of the daemon's environment,
// a harness's credential among it, reaches a binary before it is trusted.
func TestAskGivesTheBinaryNothingButHome(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "yad")
	envFile := filepath.Join(dir, "env")
	script := "#!/bin/sh\nenv > '" + envFile + "'\n[ \"$1 $2\" = 'version --json' ] || exit 2\necho '{\"version\":\"v0.9.0\",\"protocols\":[\"1\"]}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-secret")
	t.Setenv("HOME", dir)
	about, err := Ask(context.Background(), bin)
	if err != nil {
		t.Fatal(err)
	}
	if about.Version != "v0.9.0" {
		t.Errorf("about = %+v", about)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "sk-ant-secret") || !strings.Contains(string(env), "HOME="+dir) {
		t.Errorf("the binary saw:\n%s", env)
	}

	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho broken >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Ask(context.Background(), bin); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("Ask of a failing binary = %v, want its stderr in the error", err)
	}
}
