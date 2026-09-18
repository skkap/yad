package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store"
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
	// signal, and later the control socket. Nil is a runner only the hub's
	// drain control or the end of ctx can stop.
	Drain *Drain
	Log   *slog.Logger
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
	if len(o.Config.Connections) == 0 {
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

	sv := &server{drain: o.Drain, wait: o.Config.Drain.Wait.Duration, store: st, log: o.Log, reporters: map[string]*Reporter{}}
	// Every connection is set up before any goroutine starts, so the
	// executor's reporter lookup reads a map nothing writes any more.
	sv.exec = &Exec{
		Store: st, Adapters: o.Adapters, Config: o.Config, Data: o.Paths.Data, Log: o.Log,
		Report: func(conn string) {
			if r := sv.reporters[conn]; r != nil {
				r.Wake()
			}
		},
	}
	var executor Executor
	if o.Adapters != nil {
		executor = sv.exec
	}
	for _, conn := range o.Config.Connections {
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
			ClaimAfter: r.Replayed(), Log: o.Log,
		})
	}
	return sv.run(ctx)
}

// server is one Serve: its loops and reporters, the executor they share, and
// the way down.
type server struct {
	drain     *Drain
	wait      time.Duration
	store     *store.Store
	exec      *Exec
	loops     []*Loop
	reporters map[string]*Reporter
	log       *slog.Logger

	mu   sync.Mutex
	errs []error
}

// fail records a connection that stopped, and says so now: the others keep
// the process running, so the return value may be hours away.
func (s *server) fail(conn string, err error) {
	s.log.Error("connection stopped", "connection", conn, "err", err)
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
	stopped := make(chan struct{})
	go func() { wg.Wait(); close(stopped) }()

	select {
	case <-stopped:
		// Every connection stopped on its own: nothing left to sync with.
	case <-ctx.Done():
	case <-s.drain.Draining():
		s.wayDown(ctx, ended, stopped)
	}
	stopLoops()
	<-stopped
	s.exec.Wait()
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

// wayDown is the drain (decision 0029): wait for every loop to stop starting
// runs — by syncing while draining, or by stopping — and for the runs held to
// end, cancelling them once the drain wait is up or at once when asked; then
// give the reporters a bounded last chance to deliver what is owed. It returns
// early only when ctx ends: a connection that stopped, or every one of them,
// leaves the runs in hand to the same wait and the same cancel.
func (s *server) wayDown(ctx context.Context, ended []chan struct{}, stopped <-chan struct{}) {
	s.log.Warn("draining", "reason", s.drain.Reason(), "drain_wait", s.wait)
	timer := time.NewTimer(s.wait)
	defer timer.Stop()
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
	cancelling := s.drain.Cancelling()
	var idle <-chan struct{}
	for {
		select {
		case <-quiet:
			// No loop starts anything from here on, so an idle executor
			// stays idle.
			quiet, idle = nil, s.exec.Idle()
		case <-idle:
			s.settle(ctx, stopped)
			return
		case <-timer.C:
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
