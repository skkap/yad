package store

import (
	"context"
	"database/sql"
	"errors"
)

// SessionOf names the session whose workdir is dir, for a branch another
// session holds to be refused naming it. ok is false when no session has it.
func (s *Store) SessionOf(ctx context.Context, dir string) (connection, session string, ok bool, err error) {
	row, err := s.SessionByWorkdir(ctx, dir)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return row.Connection, row.ID, true, nil
}
