package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

// collectEnv is a runner store and a collector over it, on a fake clock and a
// fake disk: free space is capacity minus what the workdirs that still exist
// are said to weigh.
type collectEnv struct {
	store *store.Store
	c     *Collector
	clock *fakeClock
	root  string

	mu       sync.Mutex
	capacity int64
	weight   map[string]int64
	reclaims []string
}

const day = 24 * time.Hour

func newCollectEnv(t *testing.T) *collectEnv {
	t.Helper()
	data := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &collectEnv{
		store: st, root: filepath.Join(data, "workdirs"), weight: map[string]int64{},
		clock: &fakeClock{now: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)},
	}
	e.c = &Collector{
		Store: st, Workdirs: e.root, Clock: e.clock,
		DiskFree: func(path string) (int64, error) {
			if path != e.root {
				t.Errorf("measured %s, want the workdirs directory %s", path, e.root)
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			free := e.capacity
			for dir, w := range e.weight {
				if _, err := os.Stat(dir); err == nil {
					free -= w
				}
			}
			return free, nil
		},
		Reclaim: func(ctx context.Context, conn, id, dir string) error {
			e.mu.Lock()
			e.reclaims = append(e.reclaims, id)
			e.mu.Unlock()
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("Reclaim for %s ran after its workdir was gone: %v", id, err)
			}
			return st.FreeSlots(ctx, db.FreeSlotsParams{Connection: conn, SessionID: id})
		},
	}
	return e
}

// session makes an open session idle for idle, with a workdir holding a file
// and weighing weight on the fake disk; runState, when set, is a run held in
// it.
func (e *collectEnv) session(t *testing.T, id string, idle time.Duration, weight int64, runState string) string {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(e.root, "hub", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatal(err)
	}
	used := e.clock.Now().Add(-idle).UnixMilli()
	if idle < 0 {
		used = 0 // a stamp that is missing
	}
	if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: id, Harness: "claude", Workdir: dir, CreatedAt: 1, LastUsedAt: used}); err != nil {
		t.Fatal(err)
	}
	if runState != "" {
		e.run(t, id+"-run", id, runState)
	}
	e.mu.Lock()
	e.weight[dir] = weight
	e.mu.Unlock()
	return dir
}

func (e *collectEnv) run(t *testing.T, runID, session, state string) {
	t.Helper()
	ctx := context.Background()
	if err := e.store.CreateRun(ctx, db.CreateRunParams{Connection: "hub", ID: runID, SessionID: session, Harness: "claude", Spec: "{}", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetRunState(ctx, db.SetRunStateParams{State: state, UpdatedAt: 1, Connection: "hub", ID: runID}); err != nil {
		t.Fatal(err)
	}
}

func (e *collectEnv) get(t *testing.T, id string) db.Session {
	t.Helper()
	s, err := e.store.GetSession(context.Background(), db.GetSessionParams{Connection: "hub", ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *collectEnv) sweep(t *testing.T) {
	t.Helper()
	if err := e.c.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func gone(dir string) bool {
	_, err := os.Stat(dir)
	return errors.Is(err, os.ErrNotExist)
}

// A close with nothing held closes the session at once and records why; the
// sweep removes the workdir after Reclaim has undone what lies outside it, and
// frees the session's WT_SLOTs.
func TestCloseReclaimsTheWorkdir(t *testing.T) {
	e := newCollectEnv(t)
	ctx := context.Background()
	dir := e.session(t, "s1", time.Hour, 0, "")
	if err := e.store.TakeSlot(ctx, db.TakeSlotParams{Repo: "r", Slot: 1, Connection: "hub", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}

	res, err := e.c.Close(ctx, "hub", "s1", v1.SessionClosed)
	if err != nil || res.Outcome != CloseDone || res.Reason != v1.SessionClosed {
		t.Fatalf("close = %+v, %v", res, err)
	}
	s := e.get(t, "s1")
	if s.State != "closed" || s.CloseReason.String != "closed" || !s.ClosedAt.Valid || s.ReclaimedAt.Valid {
		t.Errorf("after close: %+v", s)
	}
	if gone(dir) {
		t.Error("the close removed the workdir itself; the sweep does that, off the caller's path")
	}

	e.sweep(t)
	if !gone(dir) {
		t.Error("the workdir is still there after the sweep")
	}
	if s := e.get(t, "s1"); !s.ReclaimedAt.Valid {
		t.Errorf("reclaimed_at not set: %+v", s)
	}
	if !slices.Equal(e.reclaims, []string{"s1"}) {
		t.Errorf("Reclaim ran for %v", e.reclaims)
	}
	if used, _ := e.store.SlotsInUse(ctx, "r"); len(used) != 0 {
		t.Errorf("slots still held: %v", used)
	}

	again, err := e.c.Close(ctx, "hub", "s1", v1.SessionClosedByOwner)
	if err != nil || again.Outcome != CloseAlready || again.Reason != v1.SessionClosed || again.ClosedAt.IsZero() {
		t.Errorf("a second close = %+v, %v; want already closed, for the first reason", again, err)
	}
	if unknown, err := e.c.Close(ctx, "hub", "nope", v1.SessionClosed); err != nil || unknown.Outcome != CloseUnknown {
		t.Errorf("closing an unknown session = %+v, %v", unknown, err)
	}
	if other, err := e.c.Close(ctx, "elsewhere", "s1", v1.SessionClosed); err != nil || other.Outcome != CloseUnknown {
		t.Errorf("another connection's close reached this one's session: %+v, %v", other, err)
	}
}

// A session with a run held is never closed, whatever the run is doing —
// waiting on a usage limit or its start time included. The close waits for
// the run to end, keeps the first reason asked for, and happens at the sweep
// after.
func TestCloseWaitsForTheRunHeld(t *testing.T) {
	for _, state := range []string{"claimed", "preparing", "running", "waiting"} {
		t.Run(state, func(t *testing.T) {
			e := newCollectEnv(t)
			ctx := context.Background()
			dir := e.session(t, "s1", time.Hour, 0, state)

			res, err := e.c.Close(ctx, "hub", "s1", v1.SessionClosedByOwner)
			if err != nil || res.Outcome != CloseWaiting || res.LiveRun != "s1-run" {
				t.Fatalf("close = %+v, %v", res, err)
			}
			if res, _ := e.c.Close(ctx, "hub", "s1", v1.SessionClosed); res.Outcome != CloseWaiting {
				t.Errorf("a repeated close = %+v", res)
			}
			e.sweep(t)
			if s := e.get(t, "s1"); s.State != "open" || !s.CloseRequestedAt.Valid || gone(dir) {
				t.Fatalf("closed with run %s: %+v", state, s)
			}

			if err := e.store.SetRunState(ctx, db.SetRunStateParams{State: "succeeded", UpdatedAt: 2, Connection: "hub", ID: "s1-run"}); err != nil {
				t.Fatal(err)
			}
			e.sweep(t)
			s := e.get(t, "s1")
			if s.State != "closed" || s.CloseReason.String != string(v1.SessionClosedByOwner) || s.CloseRequestedAt.Valid || !gone(dir) {
				t.Errorf("after the run ended: %+v, workdir gone %v", s, gone(dir))
			}
		})
	}
}

// The idle TTL expires sessions idle past it and nothing else: not one
// younger, not one with a run held however old, and not one whose last-used
// stamp is missing — that one counts from the sweep that first saw it.
func TestIdleTTL(t *testing.T) {
	e := newCollectEnv(t)
	e.c.IdleTTL = 14 * day
	old := e.session(t, "old", 15*day, 0, "")
	young := e.session(t, "young", 13*day, 0, "")
	busy := e.session(t, "busy", 30*day, 0, "waiting")
	unknown := e.session(t, "unknown", -1, 0, "")

	e.sweep(t)
	if s := e.get(t, "old"); s.State != "expired" || s.CloseReason.String != "expired" || !gone(old) {
		t.Errorf("old: %+v", s)
	}
	for id, dir := range map[string]string{"young": young, "busy": busy, "unknown": unknown} {
		if s := e.get(t, id); s.State != "open" || gone(dir) {
			t.Errorf("%s was collected: %+v", id, s)
		}
	}
	if s := e.get(t, "unknown"); s.LastUsedAt != e.clock.Now().UnixMilli() {
		t.Errorf("a missing stamp became %d, want the sweep's now %d", s.LastUsedAt, e.clock.Now().UnixMilli())
	}

	e.clock.After(14*day + time.Minute)
	e.sweep(t)
	for _, id := range []string{"young", "unknown"} {
		if s := e.get(t, id); s.State != "expired" {
			t.Errorf("%s is %s a TTL later", id, s.State)
		}
	}
	if s := e.get(t, "busy"); s.State != "open" {
		t.Errorf("busy is %s with its run still held", s.State)
	}
}

// An idle TTL of zero keeps idle sessions.
func TestIdleTTLZeroKeeps(t *testing.T) {
	e := newCollectEnv(t)
	dir := e.session(t, "old", 400*day, 0, "")
	e.sweep(t)
	if s := e.get(t, "old"); s.State != "open" || gone(dir) {
		t.Errorf("collected with no TTL: %+v", s)
	}
}

// Under the disk floor, idle sessions go longest idle first until the floor
// is met, and no further; a session with a run held, one idle less than the
// grace, and one with no workdir to free are never taken.
func TestDiskPressure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		capacity int64
		want     []string // collected, in order
	}{
		{"above the floor", 1100, nil},
		{"one frees enough", 950, []string{"oldest"}},
		{"two free enough", 850, []string{"oldest", "older"}},
		{"never enough", 100, []string{"oldest", "older", "old"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCollectEnv(t)
			e.c.DiskFloor = 500
			e.capacity = tc.capacity
			dirs := map[string]string{
				"busy":   e.session(t, "busy", 20*time.Hour, 100, "running"),
				"oldest": e.session(t, "oldest", 10*time.Hour, 100, ""),
				"older":  e.session(t, "older", 5*time.Hour, 100, ""),
				"old":    e.session(t, "old", 2*time.Hour, 100, ""),
				"recent": e.session(t, "recent", 10*time.Minute, 100, ""),
			}
			if err := e.store.CreateSession(context.Background(), db.CreateSessionParams{
				Connection: "hub", ID: "empty", Harness: "claude", CreatedAt: 1, LastUsedAt: e.clock.Now().Add(-30 * time.Hour).UnixMilli(),
			}); err != nil {
				t.Fatal(err)
			}
			e.sweep(t)
			if !slices.Equal(e.reclaims, tc.want) {
				t.Errorf("reclaimed %v, want %v", e.reclaims, tc.want)
			}
			for id, dir := range dirs {
				s := e.get(t, id)
				collected := slices.Contains(tc.want, id)
				if collected != gone(dir) || collected != (s.State == "expired") {
					t.Errorf("%s: state %s, workdir gone %v; want collected %v", id, s.State, gone(dir), collected)
				}
				if collected && s.CloseReason.String != string(v1.SessionDiskPressure) {
					t.Errorf("%s closed for %q", id, s.CloseReason.String)
				}
			}
			if s := e.get(t, "empty"); s.State != "open" {
				t.Errorf("a session with no workdir was closed for disk: %+v", s)
			}
		})
	}
}

// A removal that fails leaves the session closed and its workdir to the next
// sweep, which tries again.
func TestFailedRemovalIsRetried(t *testing.T) {
	e := newCollectEnv(t)
	ctx := context.Background()
	dir := e.session(t, "s1", time.Hour, 0, "")
	fail := true
	reclaim := e.c.Reclaim
	e.c.Reclaim = func(ctx context.Context, conn, id, dir string) error {
		if fail {
			return errors.New("git worktree remove: index.lock exists")
		}
		return reclaim(ctx, conn, id, dir)
	}
	if _, err := e.c.Close(ctx, "hub", "s1", v1.SessionClosed); err != nil {
		t.Fatal(err)
	}
	e.sweep(t)
	if s := e.get(t, "s1"); s.State != "closed" || s.ReclaimedAt.Valid || gone(dir) {
		t.Fatalf("after a failed reclaim: %+v, gone %v", s, gone(dir))
	}
	fail = false
	e.sweep(t)
	if s := e.get(t, "s1"); !s.ReclaimedAt.Valid || !gone(dir) {
		t.Errorf("after the retry: %+v, gone %v", s, gone(dir))
	}
}

// What a workdir holds is removed however it was left — read-only
// directories included — and a symlink out of it is removed without touching
// what it points at. A workdir recorded outside the workdirs directory is
// never removed.
func TestRemovalStaysInside(t *testing.T) {
	e := newCollectEnv(t)
	ctx := context.Background()
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := e.session(t, "s1", time.Hour, 0, "")
	ro := filepath.Join(dir, "pkg", "mod")
	if err := os.MkdirAll(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "go.mod"), nil, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "src")); err != nil {
		t.Fatal(err)
	}

	stray := filepath.Join(outside, "stray")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "s2", Harness: "claude", Workdir: stray, CreatedAt: 1, LastUsedAt: 1}); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"s1", "s2"} {
		if _, err := e.c.Close(ctx, "hub", id, v1.SessionClosed); err != nil {
			t.Fatal(err)
		}
	}
	e.sweep(t)
	if !gone(dir) {
		t.Error("a workdir with read-only directories in it is still there")
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "mine" {
		t.Errorf("what a symlink in the workdir pointed at: %q, %v", b, err)
	}
	if gone(stray) {
		t.Error("a directory outside the workdirs was removed")
	}
	if slices.Contains(e.reclaims, "s2") {
		t.Error("Reclaim ran for a workdir outside the workdirs")
	}
}

func TestWithin(t *testing.T) {
	for _, tc := range []struct {
		dir  string
		want bool
	}{
		{"/d/workdirs/hub/s1", true},
		{"/d/workdirs/hub", true},
		{"/d/workdirs", false},
		{"/d/workdirs/../x", false},
		{"/d/workdirs2/x", false},
		{"/d/workdirs/hub/../../x", false},
		{"relative/x", false},
		{"", false},
	} {
		if got := within("/d/workdirs", tc.dir); got != tc.want {
			t.Errorf("within(%q) = %v, want %v", tc.dir, got, tc.want)
		}
	}
}

// Whichever close is asked for first — the owner's or the hub's — is the
// reason kept, including when the second arrives after the run ended and
// before a sweep took the first; and the repeat the hub sends on every sync
// is logged once.
func TestTheFirstReasonStands(t *testing.T) {
	for _, tc := range []struct{ first, second v1.SessionCloseReason }{
		{v1.SessionClosedByOwner, v1.SessionClosed},
		{v1.SessionClosed, v1.SessionClosedByOwner},
	} {
		t.Run(string(tc.first), func(t *testing.T) {
			e := newCollectEnv(t)
			ctx := context.Background()
			var logs bytes.Buffer
			e.c.Log = slog.New(slog.NewTextHandler(&logs, nil))
			e.session(t, "s1", time.Hour, 0, "running")
			for range 3 {
				if res, err := e.c.Close(ctx, "hub", "s1", tc.first); err != nil || res.Outcome != CloseWaiting {
					t.Fatalf("close = %+v, %v", res, err)
				}
			}
			if n := strings.Count(logs.String(), "session closes when its run ends"); n != 1 {
				t.Errorf("the waiting close was logged %d times, want once:\n%s", n, logs.String())
			}
			if err := e.store.SetRunState(ctx, db.SetRunStateParams{State: "succeeded", UpdatedAt: 2, Connection: "hub", ID: "s1-run"}); err != nil {
				t.Fatal(err)
			}
			res, err := e.c.Close(ctx, "hub", "s1", tc.second)
			if err != nil || res.Outcome != CloseDone || res.Reason != tc.first {
				t.Errorf("the second close = %+v, %v; want closed for %s", res, err, tc.first)
			}
			if s := e.get(t, "s1"); s.CloseReason.String != string(tc.first) {
				t.Errorf("recorded %q, want %q", s.CloseReason.String, tc.first)
			}
		})
	}
}

// Sessions with no workdir free nothing, so they never fill disk pressure's
// page ahead of one that would.
func TestDiskPressurePassesSessionsWithNoWorkdir(t *testing.T) {
	e := newCollectEnv(t)
	ctx := context.Background()
	e.c.DiskFloor = 500
	e.capacity = 450
	for i := range sweepPage + 10 {
		if err := e.store.CreateSession(ctx, db.CreateSessionParams{
			Connection: "hub", ID: fmt.Sprintf("empty-%03d", i), Harness: "claude", CreatedAt: 1,
			LastUsedAt: e.clock.Now().Add(-100 * time.Hour).UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	dir := e.session(t, "full", 2*time.Hour, 100, "")
	e.sweep(t)
	if !gone(dir) || !slices.Equal(e.reclaims, []string{"full"}) {
		t.Errorf("reclaimed %v, workdir gone %v", e.reclaims, gone(dir))
	}
	if s := e.get(t, "empty-000"); s.State != "open" {
		t.Errorf("a session with no workdir was closed for disk: %+v", s)
	}
}
