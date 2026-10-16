package runner

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
)

// Source probing (decision 0087): each hub lists, in every sync answer, the
// repositories it may send this runner, and Sources checks each one with the
// runner's own credentials and hands the answers back to that hub's syncs.
const (
	// ReachableEvery is how often a repository found reachable is checked
	// again: a token that expires, or an account that loses access, shows
	// within it.
	ReachableEvery = 30 * time.Minute
	// UnreachableEvery is how often one found unreachable is: shorter,
	// because what usually follows an unreachable answer is its owner
	// fixing the credential — `gh auth login` — and the hub waiting to
	// route to this runner again.
	UnreachableEvery = 5 * time.Minute
	// ResendEvery is how often an answer is sent again unchanged, so a hub
	// that lost what it held — restored from a backup, or a new one behind
	// the same URL — is whole again within it.
	ResendEvery = 30 * time.Minute
	// probesAtOnce bounds the git processes probing starts together: a hub
	// listing sixty-four repositories at once is probed a few at a time
	// rather than with sixty-four ls-remotes.
	probesAtOnce = 4
)

// Sources probes the repositories every hub asks about, one answer per
// repository and ref shared between the hubs that ask, and tracks what each
// hub has been sent. Shared by every connection's loop; safe for concurrent
// use.
type Sources struct {
	// Check probes one repository: workdir.Manager.Probe.
	Check func(ctx context.Context, url, ref string) (bool, v1.SourceReachError)
	Clock Clock
	Log   *slog.Logger

	mu sync.Mutex
	// asked is each connection's list as its hub last sent it, cleaned.
	asked map[string][]v1.ProbeSource
	// results are the answers, by repository and ref; running, the probes
	// out now.
	results map[probeKey]probeResult
	running map[probeKey]bool
	// sent is, per connection, the last answer its hub took for each URL.
	sent map[string]map[string]sentReach
	wake chan struct{}
}

type probeKey struct{ url, ref string }

type probeResult struct {
	reachable bool
	class     v1.SourceReachError
	checkedAt time.Time
}

type sentReach struct {
	probeResult
	at time.Time
}

// SourceProbe is one repository a hub asked about, as `yad status` shows it.
// Checked is zero until the first probe of it ends.
type SourceProbe struct {
	URL, Ref  string
	Reachable bool
	Error     v1.SourceReachError
	Checked   time.Time
}

func (s *Sources) init() {
	if s.asked == nil {
		s.asked = map[string][]v1.ProbeSource{}
		s.results = map[probeKey]probeResult{}
		s.running = map[probeKey]bool{}
		s.sent = map[string]map[string]sentReach{}
		s.wake = make(chan struct{}, 1)
	}
	if s.Clock == nil {
		s.Clock = realClock{}
	}
	if s.Log == nil {
		s.Log = slog.New(slog.DiscardHandler)
	}
}

// Want is a hub's probe_sources: the whole list, so nil asks about nothing.
// The first of each URL counts, up to v1.ProbeSourcesMax; a URL the hub no
// longer lists is forgotten, and one it lists for the first time is probed
// at once.
func (s *Sources) Want(conn string, list []v1.ProbeSource) {
	if s == nil {
		return
	}
	var clean []v1.ProbeSource
	seen := map[string]bool{}
	for _, p := range list {
		if p.URL == "" || seen[p.URL] {
			continue
		}
		if len(clean) == v1.ProbeSourcesMax {
			break
		}
		seen[p.URL] = true
		clean = append(clean, p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if slices.Equal(clean, s.asked[conn]) {
		return
	}
	if len(list) > len(clean) && len(clean) == v1.ProbeSourcesMax {
		s.Log.Warn("the hub listed more repositories to probe than a runner takes; the first ones are probed", "connection", conn, "listed", len(list), "probed", v1.ProbeSourcesMax)
	}
	if len(clean) == 0 {
		delete(s.asked, conn)
	} else {
		s.asked[conn] = clean
	}
	for url := range s.sent[conn] {
		if !seen[url] {
			delete(s.sent[conn], url)
		}
	}
	wanted := s.wantedLocked()
	for k := range s.results {
		if !wanted[k] {
			delete(s.results, k)
		}
	}
	s.signal()
}

// wantedLocked is every repository and ref some hub asks about. s.mu is held.
func (s *Sources) wantedLocked() map[probeKey]bool {
	out := map[probeKey]bool{}
	for _, list := range s.asked {
		for _, p := range list {
			out[probeKey{p.URL, p.Ref}] = true
		}
	}
	return out
}

func (s *Sources) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Reports is what conn's next sync carries in source_reach: each answer its
// hub has not taken, or took differently, or took longer ago than
// ResendEvery and that has been checked since.
func (s *Sources) Reports(conn string) []v1.SourceReach {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	now := s.Clock.Now()
	var out []v1.SourceReach
	for _, p := range s.asked[conn] {
		r, ok := s.results[probeKey{p.URL, p.Ref}]
		if !ok {
			continue
		}
		last, sent := s.sent[conn][p.URL]
		changed := !sent || last.reachable != r.reachable || last.class != r.class
		stale := sent && r.checkedAt.After(last.checkedAt) && now.Sub(last.at) >= ResendEvery
		if changed || stale {
			out = append(out, v1.SourceReach{URL: p.URL, Reachable: r.reachable, CheckedAt: r.checkedAt.UTC(), Error: r.class})
		}
	}
	return out
}

// Reported records the answers a sync carried that its hub took with a 2xx.
func (s *Sources) Reported(conn string, sent []v1.SourceReach) {
	if s == nil || len(sent) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if s.sent[conn] == nil {
		s.sent[conn] = map[string]sentReach{}
	}
	now := s.Clock.Now()
	for _, r := range sent {
		s.sent[conn][r.URL] = sentReach{probeResult{r.Reachable, r.Error, r.CheckedAt}, now}
	}
}

// Probes is what conn's hub has asked about, with each answer so far, for
// `yad status`.
func (s *Sources) Probes(conn string) []SourceProbe {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	var out []SourceProbe
	for _, p := range s.asked[conn] {
		sp := SourceProbe{URL: p.URL, Ref: p.Ref}
		if r, ok := s.results[probeKey{p.URL, p.Ref}]; ok {
			sp.Reachable, sp.Error, sp.Checked = r.reachable, r.class, r.checkedAt
		}
		out = append(out, sp)
	}
	return out
}

// Run probes what is due until ctx ends: a repository never checked, or one
// whose answer is older than its interval.
func (s *Sources) Run(ctx context.Context) {
	s.mu.Lock()
	s.init()
	s.mu.Unlock()
	done := make(chan struct{})
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		keys, next := s.due()
		for _, k := range keys {
			wg.Go(func() {
				ok, class := s.Check(ctx, k.url, k.ref)
				s.record(k, ok, class)
				select {
				case done <- struct{}{}:
				case <-ctx.Done():
				}
			})
		}
		var timer <-chan time.Time
		if next >= 0 {
			timer = s.Clock.After(next)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-done:
		case <-timer:
		}
	}
}

// due marks running, and returns, the probes to start now — at most
// probesAtOnce out at a time — and how long until the next one not yet due
// is; negative when none is waiting on time.
func (s *Sources) due() ([]probeKey, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	now := s.Clock.Now()
	next := time.Duration(-1)
	var start []probeKey
	for k := range s.wantedLocked() {
		if s.running[k] {
			continue
		}
		at := now
		if r, ok := s.results[k]; ok {
			every := ReachableEvery
			if !r.reachable {
				every = UnreachableEvery
			}
			at = r.checkedAt.Add(every)
		}
		if wait := at.Sub(now); wait > 0 {
			if next < 0 || wait < next {
				next = wait
			}
			continue
		}
		// One left over is started when a probe out now ends, which wakes
		// the loop.
		if len(s.running) >= probesAtOnce {
			continue
		}
		s.running[k] = true
		start = append(start, k)
	}
	return start, next
}

// record keeps a probe's answer, while some hub still asks: one it stopped
// asking about while the probe ran is not news to anyone.
func (s *Sources) record(k probeKey, ok bool, class v1.SourceReachError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, k)
	if !s.wantedLocked()[k] {
		return
	}
	prev, had := s.results[k]
	s.results[k] = probeResult{ok, class, s.Clock.Now()}
	if !ok && (!had || prev.reachable || prev.class != class) {
		// Redacted though a hub never lists a URL with a credential, and
		// one that did was refused unprobed: it costs nothing.
		s.Log.Warn("a repository a hub asked about cannot be reached from this runner with its own credentials; the hub routes runs needing it elsewhere until it can",
			"url", config.RedactURL(k.url), "error", string(class))
	}
}
