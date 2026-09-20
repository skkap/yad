package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/workdir"
)

// Options are what Serve needs from the process around it.
type Options struct {
	Paths    config.Paths
	Config   config.Config
	RunnerID string
	// Capabilities returns the current document; the caller keeps it fresh.
	Capabilities func() v1.Capabilities
	// Adapters drive the runs. A runner whose registry is nil advertises no
	// capacity and claims nothing.
	Adapters *Registry
	// Drain is how the process asks the runner to go (decision 0029): a
	// signal, and the control socket's stop. Nil is a runner only the hub's
	// drain control or the end of ctx can stop.
	Drain *Drain
	Log   *slog.Logger
	// Monitor, when set, is kept current for the control socket.
	Monitor *Monitor
}

// Serve syncs every configured connection, all of them drawing on one
// capacity pool and one executor, each with its reporter delivering events and
// results. A connection whose loop stops — its credential was refused, say —
// stops alone; the others carry on, and Serve reports it when it returns.
//
// Serve returns when every connection has stopped, when a drain has run its
// course, or when ctx ends. The end of ctx is the way down's last step, exit
// now: runs in hand are killed where they stand and stay held, for the next
// start to report lost.
//
// Sharing capacity fairly between hubs is epic E7; here each sync takes
// whatever is free when it starts.
func Serve(ctx context.Context, o Options) error {
	if o.Drain == nil {
		o.Drain = NewDrain()
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	sweepGrants(o.Paths.Data, o.Log)
	if len(o.Config.Connections) == 0 {
		o.Monitor.markReady()
		select {
		case <-ctx.Done():
		case <-o.Drain.Draining():
		}
		return nil
	}
	st, err := store.Open(ctx, o.Paths.StateDB())
	if err != nil {
		return err
	}
	defer st.Close()
	pool := NewPool(o.Capabilities().Capacity)
	// One manager for preparing and reclaiming: its per-repository locks are
	// what keep a worktree being added and one being removed from meeting in
	// the same bare cache.
	w := o.Config.Workdirs
	workdirs := &workdir.Manager{
		Data: o.Paths.Data, Roots: w.EffectiveRoots(), GitTimeout: w.GitTimeout.Duration, SetupTimeout: w.SetupTimeout.Duration,
		Slots: st,
	}
	sessions := &Collector{
		Store: st, Workdirs: filepath.Join(o.Paths.Data, "workdirs"),
		IdleTTL: o.Config.Sessions.IdleTTL.Duration, DiskFloor: int64(o.Config.Sessions.DiskFloor),
		Reclaim: workdirs.Reclaim, Prune: workdirs.Prune, Log: o.Log,
	}
	o.Monitor.attach(pool, st, sessions)
	// Runs before the store closes: a status read after it would fail.
	defer o.Monitor.attach(nil, nil, nil)

	sv := &server{drain: o.Drain, wait: o.Config.Drain.Wait.Duration, store: st, log: o.Log, monitor: o.Monitor, sessions: sessions, reporters: map[string]*Reporter{}}
	// One per process, not one per connection: the runs parked on a usage
	// limit draw on the same capacity pool as everything else, and a parked
	// run must be picked up once however many hubs this runner serves.
	sv.resumer = &Resumer{Store: st, Pool: pool, Drain: o.Drain, Config: o.Config, Data: o.Paths.Data, Log: o.Log}
	sv.probe = &LoginProbe{Store: st, Config: o.Config, Data: o.Paths.Data, Log: o.Log, Freed: sv.resumer.Wake}
	// Every connection is set up before any goroutine starts, so the
	// executor's reporter lookup reads a map nothing writes any more.
	sv.exec = &Exec{
		Store: st, Adapters: o.Adapters, Config: o.Config, Data: o.Paths.Data, Workdirs: workdirs, Log: o.Log,
		Report: func(conn string) {
			if r := sv.reporters[conn]; r != nil {
				r.Wake()
			}
		},
		Ended: func() {
			sessions.Wake()
			// A run that ended gave its capacity back, which may be the
			// capacity a parked run has been waiting for.
			sv.resumer.Wake()
		},
	}
	sv.resumer.Exec = sv.exec
	var executor Executor
	if o.Adapters != nil {
		executor = sv.exec
	}
	for _, conn := range o.Config.Connections {
		o.Monitor.starting(conn.Name)
		cred, err := o.Paths.Credential(conn.Name)
		if err != nil {
			sv.fail(conn.Name, err)
			continue
		}
		client, err := hubclient.New(conn.URL, cred)
		if err != nil {
			sv.fail(conn.Name, err)
			continue
		}
		r := NewReporter(conn.Name, client, st, o.Log)
		sv.reporters[conn.Name] = r
		sv.loops = append(sv.loops, &Loop{
			Connection: conn.Name, RunnerID: o.RunnerID, Hub: client, Store: st, Pool: pool,
			Capabilities: o.Capabilities, Executor: executor, Drain: o.Drain,
			Config: o.Config, Data: o.Paths.Data, Resumer: sv.resumer,
			ClaimAfter: r.Replayed(), Log: o.Log, Monitor: o.Monitor, Sessions: sessions,
		})
	}
	return sv.run(ctx)
}

// sweepGrants deletes the grant files an earlier run left behind. They are
// 0600 files holding the secrets a hub sent, and the cleanup that removes them
// when a run ends is a deferred call in the run's goroutine: a SIGKILL, a power
// cut or an OOM kill skips it, and so does a RemoveAll that failed — a
// read-only mount, an I/O error — while the runner went on running. A crash is
// the usual reason for something to be here; it is not the only one, and the
// sweep does not need to know which.
//
// Everything under <data>/grants belongs to a run that is already over. A run's
// grants live only in the process that claimed them — the store keeps the run
// without them (Loop.record) — so no run survives a restart, and holding the
// profile's daemon lock, which `yad daemon start` takes before it reaches Serve
// (decision 0026), is what makes that true of a live daemon's runs too: there
// is no second daemon on this profile whose grants these could be.
func sweepGrants(data string, log *slog.Logger) {
	dir := filepath.Join(data, "grants")
	// Counted before the delete: a start that finds anything here has learned
	// that a hub's secrets sat on disk after their run ended, which the owner
	// would want to know. The count and nothing else — a grant's name can say
	// as much about what a hub sent as its value.
	var left int
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			left++
		}
		return nil
	})
	if err := os.RemoveAll(dir); err != nil {
		// A PathError names the file it could not unlink, which is a grant's
		// name — the one thing the line below is careful not to say. The
		// directory and the reason are what the owner acts on.
		reason := error(err)
		var pe *fs.PathError
		if errors.As(err, &pe) {
			reason = pe.Err
		}
		log.Error("grant files left by an earlier run were not removed — delete them by hand", "dir", dir, "err", reason)
		return
	}
	if left > 0 {
		log.Warn("deleted grant files an earlier run left behind: its own cleanup did not remove them, so the secrets a hub sent were on disk until now", "files", left)
	}
}

// server is one Serve: its loops and reporters, the executor they share, and
// the way down.
type server struct {
	drain     *Drain
	wait      time.Duration
	store     *store.Store
	exec      *Exec
	sessions  *Collector
	resumer   *Resumer
	probe     *LoginProbe
	loops     []*Loop
	reporters map[string]*Reporter
	log       *slog.Logger
	monitor   *Monitor

	mu   sync.Mutex
	errs []error
}

// fail records a connection that stopped, and says so now: the others keep
// the process running, so the return value may be hours away.
func (s *server) fail(conn string, err error) {
	s.log.Error("connection stopped", "connection", conn, "err", err)
	s.monitor.failed(conn, err, time.Now(), ConnStopped)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = append(s.errs, fmt.Errorf("connection %s: %w", conn, err))
}

func (s *server) run(ctx context.Context) error {
	// Loops and reporters outlive a drain's start: leases must keep renewing
	// and results keep landing until the runner is done. They end when the
	// way down does, or with ctx.
	lctx, stopLoops := context.WithCancel(ctx)
	defer stopLoops()
	var loops []*Loop
	for _, l := range s.loops {
		if err := l.Recover(ctx); err != nil {
			s.fail(l.Connection, fmt.Errorf("runs a previous process held could not be settled: %w", err))
			continue
		}
		loops = append(loops, l)
	}
	s.loops = loops
	// Collection starts once the runs a previous process held are settled,
	// and ends with the loops: the store closes when Serve returns.
	//
	// The resumer starts with it, and for the same reason: Recover has just
	// decided which of the previous process's runs are lost and which were
	// parked, so the first sweep sees the parked ones and nothing else. Its
	// runs are on ctx, not on the loops' context, exactly as a claimed run
	// is — a connection that stops must not kill a run already in hand.
	//
	// The login probe is the other half of "says when that ends": a limited
	// account comes back at its reset, and a needs-login one when the owner
	// logs it in, which nothing would otherwise notice.
	background := make(chan struct{})
	var bg sync.WaitGroup
	bg.Go(func() { s.sessions.Run(lctx) })
	if s.resumer != nil && s.exec.Adapters != nil {
		// Runs on ctx for the same reason a claimed run is: a connection
		// that stops leaves the runs in hand to finish, and only exit now
		// kills them.
		s.resumer.Runs = ctx
		// And only the connections this process actually serves, each not
		// before its own first sync. A connection dropped at start-up — an
		// unreadable credential, a Recover that failed — has no loop
		// renewing leases and no reporter delivering results, so its parked
		// runs stay parked until a process that can finish them picks them
		// up; and one that has a loop still has nothing to say about its
		// runs until the hub has answered it once.
		live := map[string]<-chan struct{}{}
		for _, l := range s.loops {
			live[l.Connection] = l.Synced()
		}
		s.resumer.Live = live
		bg.Go(func() { s.resumer.Run(lctx) })
	}
	bg.Go(func() { s.probe.Run(lctx) })
	go func() { bg.Wait(); close(background) }()
	defer func() { stopLoops(); <-background }()
	var wg sync.WaitGroup
	ended := make([]chan struct{}, len(s.loops))
	for i, l := range s.loops {
		// Runs live on ctx, not on their loop's: a connection that stops —
		// or every one of them — leaves the runs in hand to finish, and
		// only exit now kills them.
		if l.Executor != nil {
			l.Executor = runsOn{Executor: l.Executor, ctx: ctx}
		}
		ended[i] = make(chan struct{})
		// A reporter lives as long as its connection's loop: a connection
		// the owner has to fix delivers nothing, and what it owes stays in
		// the store for the next start.
		rctx, stop := context.WithCancel(lctx)
		wg.Go(func() { s.reporters[l.Connection].Run(rctx) })
		wg.Go(func() {
			defer stop()
			defer close(ended[i])
			if err := l.Run(lctx); err != nil {
				s.fail(l.Connection, err)
			}
		})
	}
	// Ready once the store is open and a loop is running: a runner whose
	// every connection failed to start is never ready.
	if len(s.loops) > 0 {
		s.monitor.markReady()
	}
	stopped := make(chan struct{})
	go func() { wg.Wait(); close(stopped) }()

	select {
	case <-stopped:
		// Every connection stopped on its own: nothing left to sync with,
		// and the runs in hand still answer the ladder on their way to an end.
	case <-ctx.Done():
	case <-s.drain.Draining():
	}
	if ctx.Err() == nil {
		s.wayDown(ctx, ended, stopped)
	}
	stopLoops()
	<-stopped
	s.exec.Wait()
	if ctx.Err() == nil && s.drain.IsDraining() {
		// A drain that ran its course is a stop someone asked for. A
		// connection that failed earlier was logged when it did; returning
		// it here would make a service manager restart the runner it was
		// asked to stop, and the hub has already cleared its request.
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.errs...)
}

// runsOn starts runs on the server's context rather than the calling loop's.
type runsOn struct {
	Executor
	ctx context.Context
}

func (r runsOn) Start(_ context.Context, c Claim) { r.Executor.Start(r.ctx, c) }

// wayDown is the drain (decision 0029), and what is left of it once every
// connection has stopped on its own: wait for every loop to stop starting
// runs — by syncing while draining, or by stopping — and for the runs held to
// end, cancelling them once the drain wait is up or at once when asked; then
// give the reporters a bounded last chance to deliver what is owed. The drain
// wait runs from the drain's start, so a runner whose connections all stopped
// still waits on its runs until a signal drains or cancels them. It returns
// early only when ctx ends.
func (s *server) wayDown(ctx context.Context, ended []chan struct{}, stopped <-chan struct{}) {
	quiet := make(chan struct{})
	go func() {
		for i, l := range s.loops {
			select {
			case <-l.Quiesced():
			case <-ended[i]:
			}
		}
		close(quiet)
	}()
	draining, cancelling := s.drain.Draining(), s.drain.Cancelling()
	var (
		idle  <-chan struct{}
		timer <-chan time.Time
	)
	for {
		select {
		case <-draining:
			draining = nil
			s.log.Warn("draining", "reason", s.drain.Reason(), "drain_wait", s.wait)
			t := time.NewTimer(s.wait)
			defer t.Stop()
			timer = t.C
		case <-quiet:
			// No loop starts anything from here on, so an idle executor
			// stays idle.
			quiet, idle = nil, s.exec.Idle()
		case <-idle:
			s.settle(ctx, stopped)
			return
		case <-timer:
			timer = nil
			s.drain.Cancel(fmt.Sprintf("the drain wait of %s ran out", s.wait))
		case <-cancelling:
			cancelling = nil
			s.exec.CancelAll(s.drain.Reason())
			if quiet != nil {
				// A loop may still start a run it had listed before the
				// drain began; that one is cancelled once the loops are
				// quiet.
				go func(quiet <-chan struct{}) {
					<-quiet
					s.exec.CancelAll(s.drain.Reason())
				}(quiet)
			}
		case <-ctx.Done():
			return
		}
	}
}

// settle waits, up to flushWait, until the spool and the outbox are empty.
// Whatever is still owed after that is replayed at the next start.
func (s *server) settle(ctx context.Context, stopped <-chan struct{}) {
	for _, r := range s.reporters {
		r.Wake()
	}
	deadline := time.NewTimer(flushWait)
	defer deadline.Stop()
	tick := time.NewTicker(settledPoll)
	defer tick.Stop()
	for {
		spool, err1 := s.store.SpoolDepth(ctx)
		outbox, err2 := s.store.OutboxDepth(ctx)
		if err1 == nil && err2 == nil && spool == 0 && outbox == 0 {
			s.log.Info("drained: every run held has ended and everything owed was delivered")
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			s.log.Warn("drained with reports still owed; the next start delivers them", "events", spool, "results", outbox)
			return
		case <-stopped:
			return
		case <-ctx.Done():
			return
		}
	}
}
