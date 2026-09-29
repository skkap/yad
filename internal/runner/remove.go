package runner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store/db"
)

// A connection the owner removed — `yad disconnect`, or an entry taken out of
// config.toml by hand — leaves what its own loop would have settled: open
// sessions and their workdirs, parked runs, runs a crashed process held,
// results and events owed. No loop of its runs again, so the runner ends
// them itself (decision 0069): at once when a running daemon is told, at the
// loop's next sync when it is not and the hub refuses the credential, and at
// the next start when no daemon ran. The hub has already done its half —
// deregister marks the runs it held lost and closes the sessions — so nothing
// here is reported to anyone.

// Removal is what a running daemon did when told a connection was removed.
type Removal struct {
	// Known is whether this daemon started with the connection configured.
	// One connected after it started was never synced here.
	Known bool
	// Already is a connection removed before this request: an earlier
	// `yad disconnect`, or its loop finding the hub refused its credential
	// and config.toml no longer listing it.
	Already bool
	// Stopped are the runs in hand that were cancelled. Their sessions
	// close once they have ended.
	Stopped []string
	ConnectionEnd
	// Remaining is how many connections this daemon still syncs.
	Remaining int
}

// ConnectionEnd is what ending one removed connection's leftovers did.
type ConnectionEnd struct {
	// Ended are runs held with no process — parked on a usage limit or a
	// start time, or left by an earlier process — now ended lost.
	Ended []string
	// Closed are sessions closed now; Closing are those that close when
	// the run in them ends.
	Closed, Closing int
}

// removedReason is what a removed connection's runs and sessions say.
func removedReason(conn string) string {
	return fmt.Sprintf("connection %s was removed from this runner, and its hub was told the runner has gone", conn)
}

// EndConnection ends what the store holds for a connection that was removed:
// its runs no process holds end lost, its sessions close — closed by the
// owner, now or once the run held in one ends — and whatever it owed its hub
// is dropped, with every close marked reported, since nobody is left to hear
// any of it. Runs in hand are left to end on their own, cancelled by whoever
// removed the connection; the sweep that follows each run's end finishes the
// job. It is safe to call again, and on a connection with nothing left.
func (c *Collector) EndConnection(ctx context.Context, conn string) (ConnectionEnd, error) {
	var out ConnectionEnd
	if c == nil {
		return out, nil
	}
	c.init()
	now := c.Clock.Now()
	held, err := c.Store.ListHeldRuns(ctx, conn)
	if err != nil {
		return out, err
	}
	why := removedReason(conn)
	for _, r := range held {
		if c.Holds != nil && c.Holds(conn, r.ID) {
			continue
		}
		n, err := c.Store.EndRemovedRun(ctx, db.EndRemovedRunParams{
			Reason: sql.NullString{String: why, Valid: true}, Now: now.UnixMilli(), Connection: conn, ID: r.ID,
		})
		if err != nil {
			return out, err
		}
		if c.Runs != nil {
			c.Runs.Forget(conn, r.ID)
		}
		if n > 0 {
			out.Ended = append(out.Ended, r.ID)
			c.Log.Warn("ended a run of a removed connection: no process holds it and no hub is left to run it for", "connection", conn, "run", r.ID, "was", r.State)
		}
	}
	sessions, err := c.Store.OpenSessionsOf(ctx, conn)
	if err != nil {
		return out, err
	}
	for _, s := range sessions {
		res, err := c.Close(ctx, conn, s.ID, v1.SessionClosedByOwner)
		if err != nil {
			return out, err
		}
		switch res.Outcome {
		case CloseDone:
			out.Closed++
		case CloseWaiting:
			out.Closing++
		}
	}
	// After the closes, so the ones just made are among those marked.
	var errs []error
	errs = append(errs, c.Store.SetConnectionSessionsReported(ctx, db.SetConnectionSessionsReportedParams{
		Now: sql.NullInt64{Int64: now.UnixMilli(), Valid: true}, Connection: conn,
	}))
	errs = append(errs, c.Store.DropConnectionOutbox(ctx, conn))
	errs = append(errs, c.Store.DropConnectionEvents(ctx, conn))
	return out, errors.Join(errs...)
}

// endRemoved is the sweep's part in it: every connection the store holds
// something of and the owner's configuration no longer lists is ended. At a
// start that is what a `yad disconnect` made with no daemon running left, and
// what a crash left of a removal the daemon was in the middle of.
func (c *Collector) endRemoved(ctx context.Context) error {
	if c.Configured == nil {
		return nil
	}
	conns, err := c.Store.ConnectionsWithLeftovers(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, conn := range conns {
		if c.Configured(conn) {
			continue
		}
		end, err := c.EndConnection(ctx, conn)
		errs = append(errs, err)
		if len(end.Ended) > 0 || end.Closed > 0 || end.Closing > 0 {
			c.Log.Info("ended what a connection no longer in config.toml left here", "connection", conn,
				"runs_ended", len(end.Ended), "sessions_closed", end.Closed, "sessions_closing", end.Closing)
		}
	}
	return errors.Join(errs...)
}

// remove is a running daemon told that the owner removed a connection from
// config.toml: its loop and reporter stop, its runs in hand are cancelled,
// and what it leaves ends (EndConnection). The other connections carry on.
func (s *server) remove(ctx context.Context, conn string) (Removal, error) {
	s.mu.Lock()
	out := Removal{Known: s.configured[conn], Already: s.removed[conn]}
	s.removed[conn] = true
	s.forget(conn)
	stop, ended := s.stops[conn], s.endedBy[conn]
	s.mu.Unlock()
	if stop == nil {
		// No loop of its own: nothing is syncing it, and the rest is safe.
		return s.finishRemoval(ctx, conn, out)
	}
	// The loop's own goroutine ends what the connection left once the loop
	// has stopped, so a loop slower than this request still has it done.
	// A repeated request waits for that stop too: what it ends before then
	// may be a claim the loop is still starting.
	stop()
	select {
	case <-ended:
	case <-ctx.Done():
		return out, fmt.Errorf("the connection's sync loop did not stop in time (%w); what it left here is ended once it has", ctx.Err())
	}
	s.mu.Lock()
	done, retired := s.retirements[conn]
	s.mu.Unlock()
	if !retired || out.Already {
		// The loop had stopped before the removal was asked — on a fault,
		// or just past its goroutine's look — so nothing retired it; or this
		// is a request again, which says what is left of it now. The loop
		// has stopped, so either is safe to do here.
		return s.finishRemoval(ctx, conn, out)
	}
	done.Removal.Known, done.Removal.Already = out.Known, out.Already
	return done.Removal, done.err
}

// retirement is what the loop's goroutine did when a removed connection's
// loop stopped, for the removal waiting on it.
type retirement struct {
	Removal
	err error
}

// retire ends what a removed connection left, from its loop's goroutine once
// the loop has stopped, and keeps the answer for the removal that asked.
func (s *server) retire(ctx context.Context, conn string) {
	// Not the loop's context, which has ended; the store is open until the
	// last loop has returned, and this runs inside one.
	out, err := s.finishRemoval(context.WithoutCancel(ctx), conn, Removal{Known: true})
	if err != nil {
		s.log.Error("not everything a removed connection left here could be ended; the collector tries again at its next sweep", "connection", conn, "err", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retirements == nil {
		s.retirements = map[string]retirement{}
	}
	s.retirements[conn] = retirement{out, err}
}

// finishRemoval is what follows a removed connection's loop stopping, however
// it stopped.
func (s *server) finishRemoval(ctx context.Context, conn string, out Removal) (Removal, error) {
	s.mu.Lock()
	if s.retired != nil {
		s.retired[conn] = true
	}
	s.mu.Unlock()
	if s.exec != nil {
		out.Stopped = s.exec.CancelConnection(conn, removedReason(conn))
	}
	// A connection nothing syncs takes no turn at the capacity.
	if s.pool != nil {
		s.pool.Pass(conn)
	}
	s.monitor.removed(conn)
	end, err := s.sessions.EndConnection(ctx, conn)
	out.ConnectionEnd = end
	out.Remaining = s.remaining()
	// For the workdirs of the sessions just closed.
	s.sessions.Wake()
	s.log.Info("connection removed: its loop stopped, and what it left here is ended", "connection", conn,
		"runs_stopped", len(out.Stopped), "runs_ended", len(end.Ended), "sessions_closed", end.Closed, "sessions_closing", end.Closing)
	return out, err
}

// removedByHub is the removal a daemon nobody told makes for itself: its
// loop was refused by the hub, and config.toml no longer lists the
// connection (goneFromConfig). The loop's goroutine retires it next.
func (s *server) removedByHub(conn string, err error) {
	s.mu.Lock()
	already := s.removed[conn]
	s.removed[conn] = true
	s.mu.Unlock()
	if !already {
		s.log.Warn("the hub refused this connection's credential and config.toml no longer lists it: the owner disconnected it, so what it left here is ended",
			"connection", conn, "err", err)
	}
}

// removalGrace is how long a loop refused by its hub waits for config.toml
// to stop listing its connection before the refusal counts as a fault.
// `yad disconnect` asks the hub first and edits config.toml a moment after,
// so a sync landing in between is refused for a connection still listed;
// called a fault, it would stop a daemon whose only hub that was, before the
// owner's command could tell it why. The edit waits on config.toml's lock
// for at most ten seconds (config.updateLockWait); a real fault reported a
// few seconds late costs nothing.
const removalGrace = 15 * time.Second

// goneFromConfig is a loop refused by its hub — the credential is unknown or
// revoked — for a connection config.toml no longer lists, now or within
// s.grace: the owner ran `yad disconnect` and the daemon was not told, so the
// refusal is the deregister taking effect, not a fault for the owner to fix.
// A connection still listed is a fault: a credential replaced or revoked at
// the hub, which `yad connect` mends and after which the hub still has the
// sessions. It gives up early, false, once the owner's command has reached
// the daemon: the removal it asked for does the rest.
func (s *server) goneFromConfig(ctx context.Context, conn string, err error) bool {
	switch hubclient.Code(err) {
	case v1.CodeUnauthorized, v1.CodeRunnerRevoked:
	default:
		return false
	}
	if s.paths.Config == "" {
		return false
	}
	deadline := time.Now().Add(s.grace)
	for {
		cfg, lerr := config.Load(s.paths)
		switch {
		case lerr != nil:
			s.log.Warn("could not read config.toml to tell a removed connection from a refused one; treating it as refused", "connection", conn, "err", lerr)
			return false
		case !slices.ContainsFunc(cfg.Connections, func(c config.Connection) bool { return c.Name == conn }):
			return !s.toldWithin(ctx, conn, tellWait)
		case s.isRemoved(conn) || !time.Now().Before(deadline):
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(graceStep):
		}
	}
}

// graceStep is how often a refused loop reads config.toml again while it
// waits: a read of a small file, a few times a second, for seconds.
const graceStep = 200 * time.Millisecond

// tellWait is how long a refused loop whose connection has left config.toml
// waits for `yad disconnect` to tell the daemon, which it does the moment it
// has deleted the credential. The owner's command then hears what was ended,
// from the removal it asked for, rather than that it had already happened.
const tellWait = 2 * time.Second

// toldWithin says whether a removal of conn reached the daemon within d.
func (s *server) toldWithin(ctx context.Context, conn string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for !s.isRemoved(conn) {
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(20 * time.Millisecond):
		}
	}
	return true
}

// isRetired says whether a removed connection's loop has stopped, so that
// what it left may be ended.
func (s *server) isRetired(conn string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retired[conn]
}

// isRemoved says whether the owner removed the connection while this daemon
// ran.
func (s *server) isRemoved(conn string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removed[conn]
}

// remaining is how many of the connections this daemon started with it still
// syncs, or would, but for a fault.
func (s *server) remaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for conn := range s.configured {
		if !s.removed[conn] {
			n++
		}
	}
	return n
}

// onlyRemoved says whether every loop that ran was stopped by a removal: the
// runner then has nothing to sync and nothing wrong with it, as one started
// with no connection.
func (s *server) onlyRemoved() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.loops {
		if !s.removed[l.Connection] {
			return false
		}
	}
	return len(s.loops) > 0
}
