// Package selfupdate is the runner keeping itself on the newest release, when
// its owner has turned that on in config.toml (decision 0071): a check every
// six hours, a download verified exactly as `yad upgrade` verifies one — it is
// internal/upgrade that does it — and a takeover at the first idle moment, or
// after a drain when none comes within a day.
//
// What this package decides is when and whether. How the process gives way to
// the new binary is the daemon's: Swap begins the drain, and the daemon
// re-executes once the drain has run its course. Nothing here is reachable
// from a hub — the setting is the owner's, and no protocol field or control
// starts a check.
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/upgrade"
)

// The owner's schedule (2026-09-30), and what it leaves to this package.
const (
	// Every is how often a runner asks whether there is a newer release. One
	// request each time, to the page `yad upgrade` reads, which is not the
	// rate-limited API (upgrade.GitHub).
	Every = 6 * time.Hour
	// First is how long after a start the first check comes: soon enough
	// that `yad status` has an answer within minutes, late enough that a
	// runner crashing at start does not ask at every restart.
	First = 10 * time.Minute
	// IdleWait is how long a downloaded release waits for an idle moment
	// before the runner drains to make one.
	IdleWait = 24 * time.Hour
	// Poll is how often a waiting release looks for the idle moment. What it
	// reads is an in-memory count, and a moment missed by a second is one a
	// sync can fill with the next run.
	Poll = time.Second
	// jitterFraction spreads a fleet's checks, so machines started together
	// do not ask together for ever.
	jitterFraction = 0.1
)

// Clock is the time the schedule is kept on; tests supply a fake.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Options is one runner's self-update.
type Options struct {
	// Source is where releases come from: the same GitHub repository `yad
	// upgrade` uses, or a fake in tests.
	Source upgrade.Source
	// Target is the binary this process was started from, which a release
	// replaces.
	Target       string
	GOOS, GOARCH string
	// Installed is this build's version, buildinfo.Version.
	Installed string
	// Needs is every protocol major a connection syncs over, with the
	// connections that do. A release must speak each one.
	Needs map[string][]string
	// Ask runs a downloaded binary's `yad version --json` and reads what it
	// says (ParseAbout).
	Ask func(ctx context.Context, path string) (buildinfo.About, error)
	// Idle reports whether nothing is in progress (runner.Monitor.Idle).
	Idle func() bool
	// Swap begins the drain the update ends in, and reports whether it did:
	// a runner already on its way down exits as it was going to, and its
	// next start is the new binary.
	Swap func(reason string) bool
	// Repo and Yad build the commands an error offers, as upgrade.Options's
	// do.
	Repo string
	Yad  func(args ...string) string
	// Every, First, IdleWait and Poll are the constants above, unless set.
	Every, First, IdleWait, Poll time.Duration
	Clock                        Clock
	// Rand returns a number in [0, 1) for jitter; nil is math/rand.
	Rand func() float64
	Log  *slog.Logger
}

// Status is where the self-update stands, for `yad status`.
type Status struct {
	// Installed is the version this process runs.
	Installed string
	// Off is why nothing is checked: a build with no release version.
	Off string
	// LastCheck is when a check last ended, and LastError why it failed —
	// empty when it succeeded. Latest is the newest release it found.
	LastCheck time.Time
	LastError string
	Latest    string
	// NextCheck is when the next check comes; zero while a release waits
	// to take over, since no check runs then.
	NextCheck time.Time
	Pending   *Pending
	Refused   *Refused
}

// Pending is a release installed on disk that this process has not yet
// become.
type Pending struct {
	Tag string
	// Since is when it was installed, and By when the runner drains for it
	// if no idle moment has come.
	Since, By time.Time
	// Swapping is set once the drain has begun.
	Swapping bool
	// Reason is why the drain began: an idle moment, or By passing.
	Reason string
}

// Refused is a newer release the runner will not take, and why. It stays
// until a later release is taken or refused in its place.
type Refused struct {
	Tag, Reason string
	At          time.Time
}

// Updater is one runner's self-update.
type Updater struct {
	o Options

	mu sync.Mutex
	st Status
}

// New returns an updater ready to Run.
func New(o Options) *Updater {
	if o.Every <= 0 {
		o.Every = Every
	}
	if o.First <= 0 {
		o.First = First
	}
	if o.IdleWait <= 0 {
		o.IdleWait = IdleWait
	}
	if o.Poll <= 0 {
		o.Poll = Poll
	}
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	if o.Rand == nil {
		o.Rand = rand.Float64
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	u := &Updater{o: o, st: Status{Installed: o.Installed}}
	if _, ok := buildinfo.ParseNumber(o.Installed); !ok {
		// Compare says Unstamped for any release: nothing could ever be
		// taken, so nothing is asked.
		u.st.Off = fmt.Sprintf("this build carries no release version (%q), so there is nothing to compare a release with — `%s` installs the newest release, which self-update then keeps current",
			o.Installed, upgrade.Command(o.Repo, o.Yad, "--force"))
	}
	return u
}

// Status is where the self-update stands now.
func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.st
	if st.Pending != nil {
		p := *st.Pending
		st.Pending = &p
	}
	if st.Refused != nil {
		r := *st.Refused
		st.Refused = &r
	}
	return st
}

// Run keeps the schedule until ctx ends or a release has been handed to Swap:
// from then on the process is on its way to becoming that release, and the
// new process keeps the schedule after it.
func (u *Updater) Run(ctx context.Context) {
	if off := u.Status().Off; off != "" {
		u.o.Log.Warn("self-update is on in config.toml and does nothing on this build", "reason", off)
		return
	}
	wait := u.jitter(u.o.First)
	for {
		u.mu.Lock()
		u.st.NextCheck = u.o.Clock.Now().Add(wait)
		u.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-u.o.Clock.After(wait):
		}
		if u.Check(ctx) {
			u.await(ctx)
			return
		}
		wait = u.jitter(u.o.Every)
	}
}

func (u *Updater) jitter(d time.Duration) time.Duration {
	return d + time.Duration((u.o.Rand()*2-1)*jitterFraction*float64(d))
}

// refusal is a release this runner will not take. It is kept apart from a
// failure: a failure is tried again at the next check, and a refusal is not,
// since the same release would be refused the same way.
type refusal struct{ reason string }

func (r refusal) Error() string { return r.reason }

// Check asks for the newest release once and, when it is newer and can be
// taken, installs it, and reports whether it did: the process then has a
// release on disk to become. A failure is logged and kept for `yad status`,
// and changes nothing else — the next check is the retry.
func (u *Updater) Check(ctx context.Context) bool {
	tag, err := u.o.Source.Latest(ctx)
	if err != nil {
		u.failed(err)
		return false
	}
	switch upgrade.Compare(u.o.Installed, tag) {
	case upgrade.Behind:
	case upgrade.UnreadableTag:
		u.failed(fmt.Errorf("the newest release's tag %q is not a version number, so there is nothing to compare this build with — nothing was installed", tag))
		return false
	default:
		u.checked(tag, nil)
		return false
	}
	u.mu.Lock()
	refused := u.st.Refused != nil && u.st.Refused.Tag == tag
	u.mu.Unlock()
	if refused {
		// Downloaded once and refused: the same bytes would be refused
		// again, so the check spends one request and not a download.
		u.checked(tag, nil)
		return false
	}
	res, err := upgrade.Apply(ctx, upgrade.Options{
		Source: u.o.Source, Target: u.o.Target, GOOS: u.o.GOOS, GOARCH: u.o.GOARCH,
		Tag: tag, Repo: u.o.Repo, Yad: u.o.Yad, Vet: u.vet(tag),
	})
	var no refusal
	switch {
	case errors.As(err, &no):
		u.checked(tag, &Refused{Tag: tag, Reason: no.reason, At: u.o.Clock.Now()})
		u.o.Log.Warn("self-update refused a newer release; this runner keeps its binary", "release", tag, "installed", u.o.Installed, "reason", no.reason)
		return false
	case err != nil:
		u.failed(err)
		return false
	}
	now := u.o.Clock.Now()
	u.mu.Lock()
	u.st.LastCheck, u.st.LastError, u.st.Latest = now, "", tag
	u.st.Refused, u.st.NextCheck = nil, time.Time{}
	u.st.Pending = &Pending{Tag: tag, Since: now, By: now.Add(u.o.IdleWait)}
	u.mu.Unlock()
	u.o.Log.Info("self-update installed a newer release; the runner becomes it at the first idle moment, or drains for it once the wait is up",
		"release", tag, "installed", u.o.Installed, "path", res.Path, "idle_wait", u.o.IdleWait)
	return true
}

func (u *Updater) checked(latest string, refused *Refused) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.st.LastCheck, u.st.LastError, u.st.Latest = u.o.Clock.Now(), "", latest
	if refused != nil {
		u.st.Refused = refused
	}
}

// failed is a check that did not get an answer, or a release that could not be
// installed: a warning in the log and in `yad status`, never a reason to stop.
func (u *Updater) failed(err error) {
	u.mu.Lock()
	u.st.LastCheck, u.st.LastError = u.o.Clock.Now(), err.Error()
	u.mu.Unlock()
	u.o.Log.Warn("self-update could not check for a newer release, or install one; it tries again at the next check", "err", err)
}

// vet asks the downloaded binary what it is before it replaces this one. The
// binary has passed its checksum by now, so what it says is the release's
// own word.
func (u *Updater) vet(tag string) func(ctx context.Context, staged string) error {
	return func(ctx context.Context, staged string) error {
		about, err := u.o.Ask(ctx, staged)
		if err != nil {
			// Not a refusal: a machine too loaded to run it in time may
			// manage at the next check.
			return fmt.Errorf("release %s's binary would not say what it is: %w — nothing was replaced", tag, err)
		}
		if upgrade.Compare(u.o.Installed, about.Version) != upgrade.Behind {
			return refusal{fmt.Sprintf("release %s's binary calls itself %q, which is not newer than this build (%s) — the release is mislabelled, and taking it would only fetch it again at every check", tag, about.Version, u.o.Installed)}
		}
		var gone []string
		for _, major := range slices.Sorted(maps.Keys(u.o.Needs)) {
			if !slices.Contains(about.Protocols, major) {
				gone = append(gone, fmt.Sprintf("v%s, which %s %s", major, strings.Join(u.o.Needs[major], ", "), syncsOver(len(u.o.Needs[major]))))
			}
		}
		if len(gone) > 0 {
			return refusal{fmt.Sprintf("release %s no longer speaks protocol %s — it speaks %s. The runner keeps %s; `%s` installs it anyway once every hub speaks what it does",
				tag, strings.Join(gone, "; nor "), protocols(about.Protocols), u.o.Installed, upgrade.Command(u.o.Repo, u.o.Yad, "--tag", tag))}
		}
		return nil
	}
}

func syncsOver(n int) string {
	if n == 1 {
		return "syncs over"
	}
	return "sync over"
}

func protocols(majors []string) string {
	if len(majors) == 0 {
		return "no protocol it names"
	}
	vs := make([]string, len(majors))
	for i, m := range majors {
		vs[i] = "v" + m
	}
	return strings.Join(vs, " and ")
}

// await waits for the first idle moment, or for the wait to run out, and
// hands the release to Swap.
func (u *Updater) await(ctx context.Context) {
	u.mu.Lock()
	by := u.st.Pending.By
	u.mu.Unlock()
	for {
		var reason string
		switch {
		case u.o.Idle():
			reason = "the runner is idle"
		case !u.o.Clock.Now().Before(by):
			reason = fmt.Sprintf("no idle moment came in %s, so the runner stops taking work and waits for the runs held to end", u.o.IdleWait)
		default:
			select {
			case <-ctx.Done():
				return
			case <-u.o.Clock.After(u.o.Poll):
			}
			continue
		}
		u.mu.Lock()
		p := u.st.Pending
		p.Swapping, p.Reason = true, reason
		u.mu.Unlock()
		if u.o.Swap(fmt.Sprintf("self-update to %s: %s", p.Tag, reason)) {
			u.o.Log.Info("self-update is taking over: no new runs; the runner re-executes as the new release once the runs held have ended", "release", p.Tag, "reason", reason)
		} else {
			u.o.Log.Info("self-update found the runner already on its way down; its next start is the new release", "release", p.Tag)
		}
		return
	}
}
