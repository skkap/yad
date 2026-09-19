//go:build unix

package workdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// lockPoll is how often a run waiting for a path looks again. flock has no
// wait that a context can cancel.
const lockPoll = 200 * time.Millisecond

// lockPaths takes a run's path sources, waiting while another run — in this
// runner, another profile's or another OS user's — holds one of them.
//
// A flock covers one directory, not the tree under it, so the locks are
// hierarchical: exclusive on each source, shared on every directory above it.
// A run on /src/app holds /src shared; a run on /src wants it exclusive and
// waits, and so does the reverse, while runs on /src/app and /src/lib go on
// side by side. Every lock a run needs is taken in one order — by path, which
// puts a directory before everything under it — so two runs can never each
// hold what the other waits for. The locks are on the directories
// themselves: every profile and user contends for the same ones, and nothing
// is written into the owner's tree. The paths are resolved and do not nest.
//
// held maps each source to the directory it locked, for the caller to confirm
// it is still the one at that path.
func lockPaths(ctx context.Context, paths []string, emit func(v1.Event)) (release func(), held map[string]*os.File, err error) {
	exclusive := map[string]bool{}
	for _, p := range paths {
		exclusive[p] = true
	}
	for _, p := range paths {
		for d := filepath.Dir(p); ; d = filepath.Dir(d) {
			if _, ok := exclusive[d]; !ok {
				exclusive[d] = false
			}
			if d == filepath.Dir(d) {
				break
			}
		}
	}
	dirs := make([]string, 0, len(exclusive))
	for d := range exclusive {
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)

	var open []*os.File
	held = map[string]*os.File{}
	release = func() {
		for _, f := range open {
			f.Close()
		}
		open = nil
	}
	waited := false
	for _, d := range dirs {
		f, err := os.Open(d)
		if err != nil {
			if exclusive[d] {
				release()
				return nil, nil, &Error{Class: ClassSourceFailed, Msg: "path lock: " + err.Error()}
			}
			// An ancestor this user cannot open is one no run of this
			// user's can name as a source either.
			continue
		}
		how := syscall.LOCK_SH
		if exclusive[d] {
			how = syscall.LOCK_EX
		}
		for {
			ok, err := tryLock(f, how)
			if err != nil {
				f.Close()
				release()
				return nil, nil, &Error{Class: ClassSourceFailed, Msg: "path lock: " + err.Error()}
			}
			if ok {
				open = append(open, f)
				if exclusive[d] {
					held[d] = f
				}
				break
			}
			if !waited {
				waited = true
				emit(v1.Event{Kind: v1.EventStatus, Status: "waiting for " + d + ", which another run is using"})
			}
			select {
			case <-ctx.Done():
				f.Close()
				release()
				return nil, nil, ctx.Err()
			case <-time.After(lockPoll):
			}
		}
	}
	return release, held, nil
}

// tryLock takes a flock without waiting. flock belongs to the open file, so
// two opens conflict even within one process, and the kernel drops the lock
// when the process dies, however it dies.
func tryLock(f *os.File, how int) (bool, error) {
	err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
