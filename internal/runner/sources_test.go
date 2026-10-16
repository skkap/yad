package runner

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
)

// stillClock is a clock a test moves by hand; nothing it hands out fires.
type stillClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stillClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stillClock) After(time.Duration) <-chan time.Time { return nil }

func (c *stillClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeReach answers probes from a table a test changes, counting each.
type fakeReach struct {
	mu      sync.Mutex
	answers map[string]v1.SourceReachError // "" is reachable
	checked []string
}

func (f *fakeReach) check(_ context.Context, url, ref string) (bool, v1.SourceReachError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked = append(f.checked, url+"@"+ref)
	class := f.answers[url]
	return class == "", class
}

func (f *fakeReach) set(url string, class v1.SourceReachError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[url] = class
}

func (f *fakeReach) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.checked)
}

func newSources(clock Clock, f *fakeReach) *Sources {
	return &Sources{Check: f.check, Clock: clock}
}

// probeDue runs every probe that is due, synchronously, as Run would.
func probeDue(s *Sources) {
	keys, _ := s.due()
	for len(keys) > 0 {
		for _, k := range keys {
			ok, class := s.Check(context.Background(), k.url, k.ref)
			s.record(k, ok, class)
		}
		keys, _ = s.due()
	}
}

func urlsOf(rs []v1.SourceReach) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.URL)
	}
	return out
}

// An answer goes to a hub once, again only when it changes or has gone
// unsent for ResendEvery, and stops when the hub stops asking.
func TestSourcesSendOnChange(t *testing.T) {
	clock := &stillClock{now: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)}
	f := &fakeReach{answers: map[string]v1.SourceReachError{"https://x/b.git": v1.ReachAuth}}
	s := newSources(clock, f)

	if got := s.Reports("hub"); got != nil {
		t.Fatalf("nothing asked, nothing answered: %v", got)
	}
	s.Want("hub", []v1.ProbeSource{{URL: "https://x/a.git"}, {URL: "https://x/b.git", Ref: "main"}})
	if got := s.Reports("hub"); len(got) != 0 {
		t.Fatalf("answered before any probe: %v", got)
	}
	probeDue(s)
	first := s.Reports("hub")
	if !slices.Equal(urlsOf(first), []string{"https://x/a.git", "https://x/b.git"}) {
		t.Fatalf("first answers = %v", first)
	}
	if !first[0].Reachable || first[0].Error != "" || first[1].Reachable || first[1].Error != v1.ReachAuth {
		t.Errorf("first answers = %+v", first)
	}
	// Not taken: sent again.
	if got := s.Reports("hub"); len(got) != 2 {
		t.Fatalf("an answer the hub did not take was dropped: %v", got)
	}
	s.Reported("hub", first)
	if got := s.Reports("hub"); len(got) != 0 {
		t.Fatalf("an answer taken was repeated: %v", got)
	}

	// The unreachable one is checked again first, unchanged: not sent.
	clock.advance(UnreachableEvery)
	probeDue(s)
	if got := s.Reports("hub"); len(got) != 0 {
		t.Errorf("an unchanged answer was sent: %v", got)
	}
	// The owner fixes the credential: the next check sends it.
	f.set("https://x/b.git", "")
	clock.advance(UnreachableEvery)
	probeDue(s)
	got := s.Reports("hub")
	if !slices.Equal(urlsOf(got), []string{"https://x/b.git"}) || !got[0].Reachable {
		t.Fatalf("after the fix = %+v", got)
	}
	s.Reported("hub", got)

	// Past ResendEvery, every answer checked since goes again.
	clock.advance(ResendEvery)
	probeDue(s)
	if got := s.Reports("hub"); len(got) != 2 {
		t.Errorf("after %s, answers sent = %v, want both", ResendEvery, got)
	}

	// The hub stops asking about one: it is neither probed nor sent.
	s.Want("hub", []v1.ProbeSource{{URL: "https://x/a.git"}})
	n := f.count()
	clock.advance(ReachableEvery)
	probeDue(s)
	if f.count() != n+1 {
		t.Errorf("probes after the list shrank = %d, want 1", f.count()-n)
	}
	for _, r := range s.Reports("hub") {
		if r.URL == "https://x/b.git" {
			t.Errorf("an answer the hub no longer asks for was sent: %+v", r)
		}
	}
	// And a hub that lists nothing has nothing probed.
	s.Want("hub", nil)
	n = f.count()
	clock.advance(ReachableEvery)
	probeDue(s)
	if f.count() != n || s.Reports("hub") != nil || s.Probes("hub") != nil {
		t.Errorf("a hub listing nothing: %d probes, reports %v, probes %v", f.count()-n, s.Reports("hub"), s.Probes("hub"))
	}
}

// Each URL counts once, the first ProbeSourcesMax of them, and one probe
// serves every hub that asks.
func TestSourcesListIsCleaned(t *testing.T) {
	clock := &stillClock{now: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)}
	f := &fakeReach{answers: map[string]v1.SourceReachError{}}
	s := newSources(clock, f)
	var list []v1.ProbeSource
	for i := range v1.ProbeSourcesMax + 5 {
		list = append(list, v1.ProbeSource{URL: fmt.Sprintf("https://x/%d.git", i)})
	}
	list = append([]v1.ProbeSource{{URL: "https://x/0.git"}, {URL: ""}}, list...)
	s.Want("one", list)
	s.Want("two", []v1.ProbeSource{{URL: "https://x/0.git"}})
	probeDue(s)
	if f.count() != v1.ProbeSourcesMax {
		t.Errorf("probes = %d, want %d", f.count(), v1.ProbeSourcesMax)
	}
	if got := len(s.Reports("one")); got != v1.ProbeSourcesMax {
		t.Errorf("answers to one = %d", got)
	}
	if got := s.Reports("two"); len(got) != 1 || !got[0].Reachable {
		t.Errorf("answers to two = %+v", got)
	}
}

// At most probesAtOnce probes are out at a time.
func TestSourcesProbeAFewAtATime(t *testing.T) {
	s := newSources(&stillClock{}, &fakeReach{answers: map[string]v1.SourceReachError{}})
	var list []v1.ProbeSource
	for i := range 10 {
		list = append(list, v1.ProbeSource{URL: fmt.Sprintf("https://x/%d.git", i)})
	}
	s.Want("hub", list)
	keys, _ := s.due()
	if len(keys) != probesAtOnce {
		t.Fatalf("started %d at once, want %d", len(keys), probesAtOnce)
	}
	if more, _ := s.due(); len(more) != 0 {
		t.Errorf("started %d more while %d were out", len(more), probesAtOnce)
	}
}

// End to end against yad hub: the hub lists the repository a run names, the
// runner answers it cannot reach it, and the run waits for a runner that can
// — this one, once its owner fixes the credential and the re-check sees it.
func TestARunWaitsForARunnerThatCanReachItsRepository(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	clock := &stillClock{now: time.Now()}
	repo := "https://git.example/acme/private.git"
	f := &fakeReach{answers: map[string]v1.SourceReachError{repo: v1.ReachAuth}}
	l.Sources = newSources(clock, f)
	if !slices.Contains(l.document().ProtocolFeatures, capability.FeatureSourceProbe) {
		t.Fatal("the runner does not advertise source_probe")
	}
	executor := l.Executor
	l.Executor = nil // no capacity offered until it has answered

	run := testRun("a", "s1")
	run.Sources = []v1.Source{{Git: &v1.GitSource{URL: repo}}}
	// For a harness this runner does not drive, so it is never offered: it
	// is here only to be left out of probe_sources.
	tokened := testRun("b", "s2")
	tokened.Harness = "codex"
	tokened.Sources = []v1.Source{{Git: &v1.GitSource{URL: "https://x-access-token:ghs_secret@git.example/acme/other.git"}}}
	e.enqueue(t, run, tokened)

	res := mustSync(t, l)
	if !slices.Equal(res.ProbeSources, []v1.ProbeSource{{URL: repo}}) {
		t.Fatalf("probe_sources = %+v; want the run's repository, and not the one with a token in it", res.ProbeSources)
	}
	probeDue(l.Sources)
	l.Executor = executor
	if res := mustSync(t, l); len(res.Runs) != 0 {
		t.Fatalf("offered %v to a runner that said it cannot reach the repository", res.Runs)
	}
	if got := e.hubState(t, "a"); got != "queued" {
		t.Fatalf("hub state = %s, want queued", got)
	}

	f.set(repo, "")
	clock.advance(UnreachableEvery)
	probeDue(l.Sources)
	res = mustSync(t, l)
	if len(res.Runs) != 1 || res.Runs[0].RunID != "a" {
		t.Fatalf("once reachable, offered %v", res.Runs)
	}
}

// `yad status` reads the probes through the monitor: an scp-like URL keeps
// its host and path rather than becoming the unprintable placeholder a hub
// URL's redaction makes of it, and a query never reaches the socket.
func TestMonitorShowsEachRepositoryItCanName(t *testing.T) {
	s := newSources(&stillClock{}, &fakeReach{answers: map[string]v1.SourceReachError{}})
	s.Want("hub", []v1.ProbeSource{{URL: "git@github.com:acme/web.git"}, {URL: "https://github.com/acme/api.git?token=secret"}})
	m := NewMonitor()
	m.attachSources(s)
	got := m.Probes("hub")
	if len(got) != 2 || got[0].URL != "redacted@github.com:acme/web.git" {
		t.Fatalf("probes = %+v; want the scp-like URL shown with its host and path", got)
	}
	if strings.Contains(got[1].URL, "secret") {
		t.Errorf("a query reached status: %q", got[1].URL)
	}
}

// A probe the way down stops is no answer: nothing is kept, and nothing is
// logged as unreachable.
func TestAProbeStoppedByTheWayDownIsNoAnswer(t *testing.T) {
	var logged bytes.Buffer
	started := make(chan struct{})
	s := &Sources{Clock: &stillClock{}, Log: slog.New(slog.NewTextHandler(&logged, nil)),
		Check: func(ctx context.Context, url, ref string) (bool, v1.SourceReachError) {
			close(started)
			<-ctx.Done()
			return false, v1.ReachFailed
		}}
	s.Want("hub", []v1.ProbeSource{{URL: "https://github.com/acme/api.git"}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	<-started
	cancel()
	<-done
	if got := s.Probes("hub"); len(got) != 1 || !got[0].Checked.IsZero() {
		t.Errorf("probes = %+v; want the repository not checked", got)
	}
	if logged.Len() != 0 {
		t.Errorf("logged on the way down: %s", logged.String())
	}
}
