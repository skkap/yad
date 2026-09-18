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
	Log      *slog.Logger
	// Monitor, when set, is kept current for the control socket.
	Monitor *Monitor
}

// Serve syncs every configured connection until ctx ends, all of them drawing
// on one capacity pool and one executor, each with its reporter delivering
// events and results. A connection whose loop stops — its credential was
// refused, say — stops alone; the others carry on, and Serve reports it when
// it returns. Serve returns once every run in hand has finished or stopped.
//
// Sharing capacity fairly between hubs is epic E7; here each sync takes
// whatever is free when it starts.
func Serve(ctx context.Context, o Options) error {
	if len(o.Config.Connections) == 0 {
		<-ctx.Done()
		return nil
	}
	st, err := store.Open(ctx, o.Paths.StateDB())
	if err != nil {
		return err
	}
	defer st.Close()
	pool := NewPool(o.Capabilities().Capacity)
	o.Monitor.attach(pool, st)
	// Runs before the store closes: a status read after it would fail.
	defer o.Monitor.attach(nil, nil)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	// fail records a connection that stopped, and says so now: the others
	// keep the process running, so the return value may be hours away.
	fail := func(conn string, err error) {
		o.Log.Error("connection stopped", "connection", conn, "err", err)
		o.Monitor.failed(conn, err, time.Now(), ConnStopped)
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, fmt.Errorf("connection %s: %w", conn, err))
	}

	// Every connection is set up before any goroutine starts, so the
	// executor's reporter lookup reads a map nothing writes any more.
	reporters := map[string]*Reporter{}
	var loops []*Loop
	var executor Executor
	exec := &Exec{
		Store: st, Adapters: o.Adapters, Config: o.Config, Data: o.Paths.Data, Log: o.Log,
		Report: func(conn string) {
			if r := reporters[conn]; r != nil {
				r.Wake()
			}
		},
	}
	if o.Adapters != nil {
		executor = exec
	}
	for _, conn := range o.Config.Connections {
		o.Monitor.starting(conn.Name)
		cred, err := o.Paths.Credential(conn.Name)
		if err != nil {
			fail(conn.Name, err)
			continue
		}
		client, err := hubclient.New(conn.URL, cred)
		if err != nil {
			fail(conn.Name, err)
			continue
		}
		reporters[conn.Name] = NewReporter(conn.Name, client, st, o.Log)
		loops = append(loops, &Loop{
			Connection: conn.Name, RunnerID: o.RunnerID, Hub: client, Store: st, Pool: pool,
			Capabilities: o.Capabilities, Executor: executor, Log: o.Log, Monitor: o.Monitor,
		})
	}
	for _, l := range loops {
		// A reporter lives as long as its connection's loop: a connection
		// the owner has to fix delivers nothing, and what it owes stays in
		// the store for the next start.
		rctx, stop := context.WithCancel(ctx)
		wg.Go(func() { reporters[l.Connection].Run(rctx) })
		wg.Go(func() {
			defer stop()
			if err := l.Run(ctx); err != nil {
				fail(l.Connection, err)
			}
		})
	}
	wg.Wait()
	exec.Wait()
	return errors.Join(errs...)
}
