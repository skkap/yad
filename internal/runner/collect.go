package runner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

// Collection's timings (decision 0035).
const (
	// sweepEvery is how often the collector looks at idle sessions and the
	// disk when nothing wakes it sooner. The idle TTL is days long; the disk
	// floor is what wants the shorter period, and a statfs and a few queries
	// every few minutes cost nothing.
	sweepEvery = 5 * time.Minute
	// diskGrace keeps disk pressure off a session whose run ended moments
	// ago: a hub continuing a conversation sends the next run within
	// minutes, and a workdir reclaimed in between costs that conversation
	// its files for the sake of one checkout's worth of space.
	diskGrace = time.Hour
	// sweepPage bounds one query's worth of idle sessions.
	sweepPage = 256
)

// Close outcomes.
const (
	// CloseDone — the session is closed; its workdir goes at the sweep this
	// wakes, and a removal that fails is tried again at the next.
	CloseDone = "closed"
	// CloseWaiting — a run is held in the session; it closes when that run
	// ends.
	CloseWaiting = "closing"
	// CloseAlready — the session was closed before, for Reason.
	CloseAlready = "already_closed"
	// CloseUnknown — this runner has no such session on that connection.
	CloseUnknown = "unknown"
)

// CloseResult is what a close did.
type CloseResult struct {
	Outcome string
	Reason  v1.SessionCloseReason
	// ClosedAt is when a closed session closed.
	ClosedAt time.Time
	// LiveRun is the run the close waits on, for CloseWaiting.
	LiveRun string
}

// Collector closes sessions and reclaims their workdirs: on the hub's
// close_session, the owner's `yad sessions close`, the idle TTL, and disk
// pressure (decisions 0011 and 0035). A session with a run held — claimed,
// preparing, running, or waiting on a usage limit or its start time — is
// never closed: its workdir is in use. The close is recorded first, in one
// statement that also checks that, and the workdir is removed after, by the
// sweep alone: removing a large checkout takes a while, and neither a sync
// nor the owner's CLI should wait on it. A removal that fails is tried again
// at the next sweep. Each connection's sync loop tells its hub of the close.
type Collector struct {
	Store *store.Store
	// Workdirs is the directory every workdir is made under. Nothing outside
	// it is ever removed, whatever a session row says.
	Workdirs string
	// IdleTTL closes sessions idle this long; zero never does.
	IdleTTL time.Duration
	// DiskFloor is the free space kept under Workdirs; zero turns it off.
	DiskFloor int64
	// Reclaim undoes what preparing the workdir left outside it — git
	// worktrees registered in the bare caches, the session's WT_SLOTs —
	// before the directory itself is removed. Nil frees the slots alone.
	Reclaim func(ctx context.Context, connection, session, dir string) error
	// DiskFree measures the free space at a path; nil is statfs.
	DiskFree func(path string) (int64, error)
	Clock    Clock
	Log      *slog.Logger

	once sync.Once
	wake chan struct{}
	// mu makes sweeps one at a time, so two of them never reclaim the same
	// workdir together.
	mu sync.Mutex
}

func (c *Collector) init() {
	c.once.Do(func() {
		c.wake = make(chan struct{}, 1)
		if c.DiskFree == nil {
			c.DiskFree = diskFree
		}
		if c.Clock == nil {
			c.Clock = realClock{}
		}
		if c.Log == nil {
			c.Log = slog.New(slog.DiscardHandler)
		}
	})
}

// Wake asks for a sweep now: a run ended, and a close may have been waiting
// on it. Safe on a nil collector.
func (c *Collector) Wake() {
	if c == nil {
		return
	}
	c.init()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Run sweeps until ctx ends: at once, whenever woken, and every sweepEvery.
// A nil collector returns at once.
func (c *Collector) Run(ctx context.Context) {
	if c == nil {
		return
	}
	c.init()
	for {
		if err := c.Sweep(ctx); err != nil && ctx.Err() == nil {
			c.Log.Error("workdir collection failed; trying again at the next sweep", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-c.Clock.After(sweepEvery):
		}
	}
}

// Close closes a session for reason: now, when no run is held in it, and
// otherwise as soon as the run held ends. The first reason asked for is the
// one recorded.
func (c *Collector) Close(ctx context.Context, connection, id string, reason v1.SessionCloseReason) (CloseResult, error) {
	c.init()
	sess, err := c.Store.GetSession(ctx, db.GetSessionParams{Connection: connection, ID: id})
	if errors.Is(err, sql.ErrNoRows) {
		return CloseResult{Outcome: CloseUnknown}, nil
	}
	if err != nil {
		return CloseResult{}, err
	}
	if sess.State != "open" {
		return CloseResult{Outcome: CloseAlready, Reason: v1.SessionCloseReason(sess.CloseReason.String), ClosedAt: msTime(sess.ClosedAt.Int64)}, nil
	}
	closed, err := c.closeNow(ctx, sess, reason, false)
	if err != nil {
		return CloseResult{}, err
	}
	if closed {
		c.Wake()
		return CloseResult{Outcome: CloseDone, Reason: reason, ClosedAt: c.Clock.Now().UTC()}, nil
	}
	now := c.Clock.Now().UnixMilli()
	if err := c.Store.RequestSessionClose(ctx, db.RequestSessionCloseParams{
		Now:    sql.NullInt64{Int64: now, Valid: true},
		Reason: sql.NullString{String: string(reason), Valid: true}, Connection: connection, ID: id,
	}); err != nil {
		return CloseResult{}, err
	}
	live, err := c.Store.HeldRunInSession(ctx, db.HeldRunInSessionParams{Connection: connection, ID: id})
	if err != nil {
		return CloseResult{}, err
	}
	if live == "" {
		// The run ended between the close and the request: the next sweep
		// finds the request with nothing held, and closes it.
		c.Wake()
	}
	c.Log.Info("session closes when its run ends", "connection", connection, "session", id, "reason", reason, "run", live)
	return CloseResult{Outcome: CloseWaiting, LiveRun: live}, nil
}

// Sweep does every close that is due: those asked for while a run was held,
// the idle TTL's, and disk pressure's; then removes any workdir a previous
// sweep closed and could not remove.
func (c *Collector) Sweep(ctx context.Context) error {
	c.init()
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.Clock.Now()
	if n, err := c.Store.StampUnknownLastUsed(ctx, now.UnixMilli()); err != nil {
		return err
	} else if n > 0 {
		c.Log.Warn("sessions with no last-used time; their idle time counts from now", "sessions", n)
	}
	var errs []error
	asked, err := c.Store.SessionsCloseRequested(ctx)
	if err != nil {
		return err
	}
	for _, s := range asked {
		_, err := c.closeNow(ctx, s, v1.SessionCloseReason(s.CloseReason.String), true)
		errs = append(errs, err)
	}
	if c.IdleTTL > 0 {
		errs = append(errs, c.expire(ctx, now))
	}
	if c.DiskFloor > 0 {
		errs = append(errs, c.relieve(ctx, now))
	}
	left, err := c.Store.UnreclaimedSessions(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, s := range left {
		c.reclaim(ctx, s)
	}
	return errors.Join(errs...)
}

// expire closes every session idle past the TTL.
func (c *Collector) expire(ctx context.Context, now time.Time) error {
	for {
		idle, err := c.Store.IdleSessions(ctx, db.IdleSessionsParams{IdleSince: now.Add(-c.IdleTTL).UnixMilli(), Max: sweepPage})
		if err != nil {
			return err
		}
		closed := 0
		for _, s := range idle {
			ok, err := c.closeNow(ctx, s, v1.SessionExpired, true)
			if err != nil {
				return err
			}
			if ok {
				closed++
			}
		}
		// A page that closed nothing would come back the same.
		if len(idle) < sweepPage || closed == 0 {
			return nil
		}
	}
}

// relieve closes idle sessions, longest idle first, until the disk under the
// workdirs is back above the floor or no candidate is left. A session with no
// workdir yet frees nothing and is left alone.
func (c *Collector) relieve(ctx context.Context, now time.Time) error {
	free, err := c.DiskFree(c.Workdirs)
	if err != nil {
		return fmt.Errorf("measuring the free space under %s: %w", c.Workdirs, err)
	}
	if free >= c.DiskFloor {
		return nil
	}
	idle, err := c.Store.IdleSessions(ctx, db.IdleSessionsParams{IdleSince: now.Add(-diskGrace).UnixMilli(), Max: sweepPage})
	if err != nil {
		return err
	}
	for _, s := range idle {
		if s.Workdir == "" {
			continue
		}
		c.Log.Warn("the disk under the workdirs is below its floor; closing the longest idle session",
			"free_bytes", free, "floor_bytes", c.DiskFloor, "connection", s.Connection, "session", s.ID)
		if _, err := c.closeNow(ctx, s, v1.SessionDiskPressure, true); err != nil {
			return err
		}
		if free, err = c.DiskFree(c.Workdirs); err != nil {
			return fmt.Errorf("measuring the free space under %s: %w", c.Workdirs, err)
		}
		if free >= c.DiskFloor {
			return nil
		}
	}
	c.Log.Warn("the disk under the workdirs is below its floor and no idle session is left to reclaim — free space on it, or lower sessions.disk_floor",
		"free_bytes", free, "floor_bytes", c.DiskFloor, "workdirs", c.Workdirs)
	return nil
}

// closeNow closes a session unless a run is held in it, and with reclaim —
// which only a sweep passes — reclaims its workdir. It reports whether it
// closed it.
func (c *Collector) closeNow(ctx context.Context, s db.Session, reason v1.SessionCloseReason, reclaim bool) (bool, error) {
	state := "closed"
	if reason == v1.SessionExpired || reason == v1.SessionDiskPressure {
		state = "expired"
	}
	now := c.Clock.Now().UnixMilli()
	n, err := c.Store.CloseSession(ctx, db.CloseSessionParams{
		State: state, Reason: sql.NullString{String: string(reason), Valid: true},
		Now: sql.NullInt64{Int64: now, Valid: true}, Connection: s.Connection, ID: s.ID,
	})
	if err != nil || n == 0 {
		return false, err
	}
	c.Log.Info("session closed", "connection", s.Connection, "session", s.ID, "reason", reason,
		"idle", time.Duration(now-s.LastUsedAt)*time.Millisecond)
	if reclaim {
		s.State = state
		c.reclaim(ctx, s)
	}
	return true, nil
}

// reclaim removes a closed session's workdir and what it held outside it,
// and records that it is gone. A failure is logged and left for the next
// sweep, which tries again.
func (c *Collector) reclaim(ctx context.Context, s db.Session) {
	log := c.Log.With("connection", s.Connection, "session", s.ID, "workdir", s.Workdir)
	switch {
	case s.Workdir == "":
		// Closed before any run of it reached its workdir: nothing on disk,
		// and no slot either, unless one was taken without one.
		if err := c.Store.FreeSlots(ctx, db.FreeSlotsParams{Connection: s.Connection, SessionID: s.ID}); err != nil {
			log.Error("the session's WT_SLOTs were not freed; trying again at the next sweep", "err", err)
			return
		}
	case !within(c.Workdirs, s.Workdir):
		// Never true of a path the executor recorded. Removing a directory
		// outside the workdirs on a database row's say-so is not something
		// to get wrong once.
		log.Error("the session's workdir is not under the workdirs directory and is left in place — remove it by hand if it is not needed", "workdirs", c.Workdirs)
	default:
		if err := c.reclaimOutside(ctx, s); err != nil {
			log.Error("the session's worktrees were not removed; trying again at the next sweep", "err", err)
			return
		}
		if err := removeAll(s.Workdir); err != nil {
			log.Error("the session's workdir was not removed; trying again at the next sweep", "err", err)
			return
		}
		log.Info("workdir reclaimed")
	}
	if err := c.Store.SetSessionReclaimed(ctx, db.SetSessionReclaimedParams{
		ReclaimedAt: sql.NullInt64{Int64: c.Clock.Now().UnixMilli(), Valid: true}, Connection: s.Connection, ID: s.ID,
	}); err != nil {
		log.Error("the workdir is gone and the store does not say so; the next sweep finds nothing to remove", "err", err)
	}
}

func (c *Collector) reclaimOutside(ctx context.Context, s db.Session) error {
	if c.Reclaim != nil {
		return c.Reclaim(ctx, s.Connection, s.ID, s.Workdir)
	}
	return c.Store.FreeSlots(ctx, db.FreeSlotsParams{Connection: s.Connection, SessionID: s.ID})
}

// FreeBytes is the free space under the workdirs, for the health report; 0
// when it cannot be measured. Safe on a nil collector.
func (c *Collector) FreeBytes() int64 {
	if c == nil {
		return 0
	}
	c.init()
	n, err := c.DiskFree(c.Workdirs)
	if err != nil {
		return 0
	}
	return n
}

// within reports whether dir lies strictly inside root.
func within(root, dir string) bool {
	if root == "" || !filepath.IsAbs(dir) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(dir))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// removeAll is os.RemoveAll that also gets through directories a harness left
// read-only — Go's module cache marks its own that way — by making them
// writable first. It never follows a symlink: a link out of the workdir is
// removed, and what it points at is not touched.
func removeAll(dir string) error {
	err := os.RemoveAll(dir)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

func msTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
