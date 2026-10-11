package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/skkap/yad/internal/store/db"
)

// RetiredSeparator joins a connection's name to the moment its rows were
// retired. It is outside the characters a connection name may use
// (config.ValidName), so a retired name never matches a configured one, and
// the collector ends whatever such a name still holds as it ends any removed
// connection's.
const RetiredSeparator = "~"

// Retirement is what AdoptRegistration moved: the retired name the rows went
// to, empty when nothing moved, and how many sessions and runs went.
type Retirement struct {
	As             string
	Sessions, Runs int64
}

// AdoptRegistration makes the rows under conn the registration config.toml
// names (decision 0079). registeredAt is the connection entry's registered_at:
// when it is not the one recorded here, the name was given to a new
// registration since — a different hub, or the same one after a disconnect —
// and every row an earlier one left under the name is moved to
// <conn>~<unix ms> before anything of the new one is written. A connection
// made before config.toml carried the field (0) is adopted with what it has.
//
// One transaction: a crash leaves the name's rows all moved and the new
// registration recorded, or neither. The foreign keys are deferred to its
// commit, since every table refers to the sessions being renamed.
func (s *Store) AdoptRegistration(ctx context.Context, conn string, registeredAt int64, now time.Time) (Retirement, error) {
	var out Retirement
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	err = adopt(ctx, tx, s.Queries.WithTx(tx), conn, registeredAt, now, &out)
	if err != nil {
		return Retirement{}, errors.Join(err, tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return Retirement{}, err
	}
	return out, nil
}

func adopt(ctx context.Context, tx *sql.Tx, q *db.Queries, conn string, registeredAt int64, now time.Time, out *Retirement) error {
	stored, err := q.ConnectionRegistration(ctx, conn)
	known := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if known && stored == registeredAt {
		return nil
	}
	// No registered_at says nothing about which hub the rows came from, so
	// nothing is moved on its say-so: an upgrade must not retire the
	// sessions of a hub that is still connected.
	if registeredAt != 0 {
		has, err := q.ConnectionHasSessions(ctx, conn)
		if err != nil {
			return err
		}
		if has {
			if err := retire(ctx, tx, q, conn, now, out); err != nil {
				return err
			}
		}
	}
	return q.SetConnectionRegistration(ctx, db.SetConnectionRegistrationParams{Connection: conn, RegisteredAt: registeredAt})
}

func retire(ctx context.Context, tx *sql.Tx, q *db.Queries, conn string, now time.Time, out *Retirement) error {
	if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys = ON"); err != nil {
		return err
	}
	// Two retirements of one name in one millisecond would share a name and
	// collide on its sessions' keys, so the moment moves on until it is free.
	for at := now.UnixMilli(); ; at++ {
		out.As = conn + RetiredSeparator + strconv.FormatInt(at, 10)
		taken, err := q.ConnectionHasSessions(ctx, out.As)
		if err != nil {
			return err
		}
		if !taken {
			break
		}
	}
	var err error
	if out.Sessions, err = q.RetireConnectionSessions(ctx, db.RetireConnectionSessionsParams{Retired: out.As, Connection: conn}); err != nil {
		return fmt.Errorf("retire the sessions under %q: %w", conn, err)
	}
	if out.Runs, err = q.RetireConnectionRuns(ctx, db.RetireConnectionRunsParams{Retired: out.As, Connection: conn}); err != nil {
		return fmt.Errorf("retire the runs under %q: %w", conn, err)
	}
	if err := q.RetireConnectionEvents(ctx, db.RetireConnectionEventsParams{Retired: out.As, Connection: conn}); err != nil {
		return fmt.Errorf("retire the events under %q: %w", conn, err)
	}
	if err := q.RetireConnectionOutbox(ctx, db.RetireConnectionOutboxParams{Retired: out.As, Connection: conn}); err != nil {
		return fmt.Errorf("retire the results owed under %q: %w", conn, err)
	}
	if err := q.RetireConnectionSlots(ctx, db.RetireConnectionSlotsParams{Retired: out.As, Connection: conn}); err != nil {
		return fmt.Errorf("retire the WT_SLOTs under %q: %w", conn, err)
	}
	return nil
}
