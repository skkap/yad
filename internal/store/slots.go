package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/skkap/yad/internal/store/db"
)

// Slot returns the WT_SLOT a session holds for a repository, allocating the
// smallest free one on first use. Slots start at 1, as gpiwt's do: a setup
// hook derives ports from the slot, and slot 0 is a main checkout's in that
// contract.
//
// The session keeps its slot for as long as it keeps its worktree, so a hook
// run again in the same workdir derives the same ports.
func (s *Store) Slot(ctx context.Context, repo, connection, session string) (int64, error) {
	var slot int64
	err := s.Tx(ctx, func(q *db.Queries) error {
		held, err := q.SessionSlot(ctx, db.SessionSlotParams{Repo: repo, Connection: connection, SessionID: session})
		if err == nil {
			slot = held
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		used, err := q.SlotsInUse(ctx, repo)
		if err != nil {
			return err
		}
		// used is sorted, so the first gap in 1, 2, 3 … is the smallest free.
		slot = 1
		for _, u := range used {
			if u == slot {
				slot++
			} else if u > slot {
				break
			}
		}
		return q.TakeSlot(ctx, db.TakeSlotParams{Repo: repo, Slot: slot, Connection: connection, SessionID: session})
	})
	return slot, err
}

// ReleaseSlots frees every slot a session holds, for a workdir being
// reclaimed; the next worktree of the same repository may reuse them.
func (s *Store) ReleaseSlots(ctx context.Context, connection, session string) error {
	return s.FreeSlots(ctx, db.FreeSlotsParams{Connection: connection, SessionID: session})
}
