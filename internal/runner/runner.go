package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

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
	Executor     Executor
	Log          *slog.Logger
}

// Serve syncs every configured connection until ctx ends, all of them drawing
// on one capacity pool. A connection whose loop stops — its credential was
// refused, say — stops alone; the others carry on, and Serve reports it when
// it returns.
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

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	// fail records a connection that stopped, and says so now: the others
	// keep the process running, so the return value may be hours away.
	fail := func(conn string, err error) {
		o.Log.Error("connection stopped", "connection", conn, "err", err)
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, fmt.Errorf("connection %s: %w", conn, err))
	}
	for _, conn := range o.Config.Connections {
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
		l := &Loop{
			Connection: conn.Name, RunnerID: o.RunnerID, Hub: client, Store: st, Pool: pool,
			Capabilities: o.Capabilities, Executor: o.Executor, Log: o.Log,
		}
		wg.Go(func() {
			if err := l.Run(ctx); err != nil {
				fail(conn.Name, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
